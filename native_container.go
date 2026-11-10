package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// This image supplies glibc only; native jobs never invoke its Node engine.
const nativeImage = "node@sha256:78bc820a7a6148e8c48731eca18dbb9b6b2c05ac4a52138479be565f2831682d"

func probeNativeContainer(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.OSType}} {{.Architecture}} {{.CgroupVersion}}").Output()
	if err != nil {
		return fmt.Errorf("Docker unavailable: %w", err)
	}
	if strings.TrimSpace(string(out)) != "linux aarch64 2" {
		return errors.New("native Shape requires Linux arm64 Docker with cgroup v2")
	}
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", nativeImage).Run(); err != nil {
		return errors.New("pinned native base image is unavailable; install the documented image")
	}
	return nil
}

func nativeContainerArgs(name, runtimeDir, inputDir, outputDir string) []string {
	return []string{"create", "--name", name, "--label", "io.deltatronix.shape-native=true", "--label", "io.deltatronix.shape-owner=" + nativeContainerOwner(name), "--platform", "linux/arm64", "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--memory", "1073741824", "--memory-swap", "1073741824", "--cpus", "1", "--pids-limit", "64", "--ulimit", "core=0:0", "--ulimit", "fsize=8388608:8388608", "--tmpfs", "/tmp:rw,noexec,nosuid,size=16777216", "--mount", "type=bind,src=" + runtimeDir + ",dst=/runtime,readonly", "--mount", "type=bind,src=" + inputDir + ",dst=/input,readonly", "--mount", "type=bind,src=" + outputDir + ",dst=/output", "--log-driver", "none", "--interactive", "--entrypoint", "/runtime/shape-native", nativeImage, "/input/job.json"}
}

type boundedNativeBuffer struct {
	mu       sync.Mutex
	b        bytes.Buffer
	max      int
	overflow context.CancelFunc
}

func (b *boundedNativeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len()+len(p) > b.max {
		b.overflow()
		return 0, errors.New("native output limit exceeded")
	}
	return b.b.Write(p)
}
func (b *boundedNativeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.b.Bytes()...)
}

// Container removal uses a separate context: cancellation must not prevent kill.
func removeNativeContainer(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remove native container: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runNativeContainer(ctx context.Context, name, runtimeDir, inputDir, outputDir, requestID string) (stdout, stderr []byte, receipt map[string]any, err error) {
	for _, p := range []string{runtimeDir, inputDir, outputDir} {
		if !filepath.IsAbs(p) || strings.Contains(p, ",") {
			return nil, nil, nil, errors.New("invalid native mount path")
		}
	}
	if err = probeNativeContainer(ctx); err != nil {
		return
	}
	startup, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	out, createErr := exec.CommandContext(startup, "docker", nativeContainerArgs(name, runtimeDir, inputDir, outputDir)...).CombinedOutput()
	cancelStartup()
	if createErr != nil {
		_ = removeNativeContainer(name)
		return nil, nil, nil, fmt.Errorf("create native container: %w: %s", createErr, out)
	}
	defer func() {
		if err == nil {
			receipt, err = inspectNativeReceipt(name)
		}
		if cleanupErr := removeNativeContainer(name); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		} else if receipt != nil {
			receipt["cleanupVerified"] = true
		}
	}()
	work, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stdoutBuffer := &boundedNativeBuffer{max: 12 << 20, overflow: cancel}
	stderrBuffer := &boundedNativeBuffer{max: 64 << 10, overflow: cancel}
	cmd := exec.Command("docker", "start", "--attach", "--interactive", name)
	stdin, pipeErr := cmd.StdinPipe()
	if pipeErr != nil {
		return nil, nil, nil, pipeErr
	}
	cmd.Stdout = stdoutBuffer
	cmd.Stderr = stderrBuffer
	if err = cmd.Start(); err != nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-work.Done():
		cancelFrame, _ := json.Marshal(map[string]any{"contractVersion": 1, "type": "cancel", "requestId": requestID})
		_, _ = io.WriteString(stdin, string(cancelFrame)+"\n")
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			cleanupErr := removeNativeContainer(name)
			if cleanupErr != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
		err = work.Err()
	}
	_ = stdin.Close()
	stdout = stdoutBuffer.Bytes()
	stderr = stderrBuffer.Bytes()
	return
}

func validateNativeResultLines(data []byte) (json.RawMessage, error) {
	var result json.RawMessage
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > 256<<10 {
			return nil, errors.New("native result line exceeds limit")
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			return nil, errors.New("invalid native JSONL")
		}
		switch frame.Type {
		case "limits", "phase":
		case "refused":
			return nil, fmt.Errorf("native runtime refused: %s", line)
		case "result":
			if result != nil {
				return nil, errors.New("duplicate native result")
			}
			result = append(json.RawMessage(nil), line...)
		default:
			return nil, errors.New("unknown native output frame")
		}
	}
	if result == nil {
		return nil, errors.New("native runtime produced no result")
	}
	return result, nil
}

func inspectNativeReceipt(name string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "docker", "inspect", name).Output()
	if err != nil {
		return nil, err
	}
	var entries []struct {
		State struct {
			OOMKilled bool
			ExitCode  int
		}
		Config struct {
			User  string
			Image string
		}
		HostConfig struct {
			Memory         int64
			MemorySwap     int64
			NanoCpus       int64
			PidsLimit      int64
			ReadonlyRootfs bool
			NetworkMode    string
			LogConfig      struct{ Type string }
		}
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	if len(entries) != 1 {
		return nil, errors.New("missing native container evidence")
	}
	e := entries[0]
	h := e.HostConfig
	if e.Config.Image != nativeImage || e.State.OOMKilled || e.State.ExitCode != 0 || h.Memory != 1<<30 || h.MemorySwap != 1<<30 || h.NanoCpus != 1000000000 || h.PidsLimit != 64 || !h.ReadonlyRootfs || h.NetworkMode != "none" || e.Config.User != "65534:65534" || h.LogConfig.Type != "none" {
		return nil, errors.New("native container did not satisfy hard limits")
	}
	return map[string]any{"status": "completed", "image": strings.TrimPrefix(nativeImage, "node@"), "container": map[string]any{"oom": e.State.OOMKilled, "exit": e.State.ExitCode, "memory": h.Memory, "swap": h.MemorySwap, "nano": h.NanoCpus, "pids": h.PidsLimit, "readonly": h.ReadonlyRootfs, "network": h.NetworkMode, "user": e.Config.User, "logDriver": h.LogConfig.Type}, "cleanupVerified": false, "hardKilled": false, "cooperativeCancelled": false}, nil
}

func nativeContainerOwner(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) >= 4 {
		return parts[2]
	}
	return "test"
}
func reconcileNativeContainers(ctx context.Context, agentID string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	owner := hashNative([]byte(agentID))[:12]
	out, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet", "--filter", "label=io.deltatronix.shape-native=true", "--filter", "label=io.deltatronix.shape-owner="+owner).Output()
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(out)) {
		if err := removeNativeContainer(id); err != nil {
			return err
		}
	}
	return nil
}
