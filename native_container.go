package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	observe  func([]byte) error
}

func (b *boundedNativeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.b.Len()+len(p) > b.max {
		b.overflow()
		return 0, errors.New("native output limit exceeded")
	}
	if b.observe != nil {
		if err := b.observe(p); err != nil {
			b.overflow()
			return 0, err
		}
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
	receipt = map[string]any{"cleanupVerified": true}
	for _, p := range []string{runtimeDir, inputDir, outputDir} {
		if !filepath.IsAbs(p) || strings.Contains(p, ",") {
			return nil, nil, receipt, errors.New("invalid native mount path")
		}
	}
	startupDeadline := time.Now().Add(30 * time.Second)
	if err = probeNativeContainer(ctx); err != nil {
		return
	}
	startup, cancelStartup := context.WithDeadline(ctx, startupDeadline)
	out, createErr := exec.CommandContext(startup, "docker", nativeContainerArgs(name, runtimeDir, inputDir, outputDir)...).CombinedOutput()
	cancelStartup()
	if createErr != nil {
		cleanupErr := removeNativeContainer(name)
		receipt["cleanupVerified"] = cleanupErr == nil
		return nil, nil, receipt, fmt.Errorf("create native container: %w: %s", errors.Join(createErr, cleanupErr), out)
	}
	receipt["cleanupVerified"] = false
	containerRemoved := false
	defer func() {
		if err == nil {
			receipt, err = inspectNativeReceipt(name)
		}
		if !containerRemoved {
			if cleanupErr := removeNativeContainer(name); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			} else {
				containerRemoved = true
			}
		}
		if containerRemoved {
			if receipt == nil {
				receipt = map[string]any{}
			}
			receipt["cleanupVerified"] = true
		}
	}()
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	running := make(chan struct{}, 1)
	monitor := &nativeOutputMonitor{requestID: requestID, running: running}
	stdoutBuffer := &boundedNativeBuffer{max: 12 << 20, overflow: cancel, observe: monitor.write}
	stderrBuffer := &boundedNativeBuffer{max: 64 << 10, overflow: cancel}
	cmd := exec.Command("docker", "start", "--attach", "--interactive", name)
	stdin, pipeErr := cmd.StdinPipe()
	if pipeErr != nil {
		return nil, nil, receipt, pipeErr
	}
	cmd.Stdout = stdoutBuffer
	cmd.Stderr = stderrBuffer
	if err = cmd.Start(); err != nil {
		return
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	controlsCtx, controlsCancel := context.WithDeadline(work, startupDeadline)
	defer controlsCancel()
	controlsDone := make(chan error, 1)
	go func() {
		controlsErr := publishNativeControls(controlsCtx, name, inputDir, requestID)
		controlsDone <- controlsErr
		if controlsErr != nil {
			cancel()
		}
	}()

	exited, waitErr := waitNativeExecution(work, done, running, time.Until(startupDeadline), 60*time.Second)
	err = waitErr
	if !exited {
		cancelFrame, _ := json.Marshal(map[string]any{"contractVersion": 1, "type": "cancel", "requestId": requestID})
		_, _ = io.WriteString(stdin, string(cancelFrame)+"\n")
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			cleanupErr := removeNativeContainer(name)
			if cleanupErr == nil {
				containerRemoved = true
			}
			if cleanupErr != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	}

	controlsCancel()
	controlsErr := <-controlsDone
	if controlsErr != nil && err == nil {
		err = controlsErr
	}
	_ = stdin.Close()
	if err == nil && len(monitor.pending) > 0 {
		err = errors.New("incomplete native JSONL frame")
	}
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

// The work clock begins only after the child reports a validated running phase.
func waitNativeExecution(ctx context.Context, done <-chan error, running <-chan struct{}, startup, wall time.Duration) (bool, error) {
	timer := time.NewTimer(startup)
	defer timer.Stop()
	phaseRunning := false
	for {
		select {
		case err := <-done:
			return true, err
		case <-ctx.Done():
			return false, ctx.Err()
		case <-running:
			if phaseRunning {
				return false, errors.New("duplicate native running phase")
			}
			phaseRunning = true
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(wall)
		case <-timer.C:
			if phaseRunning {
				return false, errors.New("native work timeout")
			}
			return false, errors.New("native startup timeout")
		}
	}
}

type nativeOutputMonitor struct {
	requestID string
	running   chan<- struct{}
	pending   []byte
	limits    bool
	booting   bool
	started   bool
	terminal  bool
}

func (m *nativeOutputMonitor) write(p []byte) error {
	m.pending = append(m.pending, p...)
	for {
		end := bytes.IndexByte(m.pending, '\n')
		if end < 0 {
			if len(m.pending) > 256<<10 {
				return errors.New("native JSONL line exceeds limit")
			}
			return nil
		}
		if end > 256<<10 {
			return errors.New("native JSONL line exceeds limit")
		}
		line := m.pending[:end]
		m.pending = m.pending[end+1:]
		var f struct {
			ContractVersion int            `json:"contractVersion"`
			RequestID       string         `json:"requestId"`
			Type            string         `json:"type"`
			Phase           string         `json:"phase"`
			Applied         map[string]any `json:"applied"`
		}
		if err := json.Unmarshal(line, &f); err != nil {
			return errors.New("invalid native JSONL")
		}
		if f.ContractVersion != 1 || f.RequestID != m.requestID || m.terminal {
			return errors.New("native output identity/order mismatch")
		}
		switch f.Type {
		case "limits":
			if !nativeAppliedValid(f.Applied) {
				return errors.New("native applied limits mismatch")
			}
			if m.limits || m.booting {
				return errors.New("duplicate native limits")
			}
			m.limits = true
		case "phase":
			if !m.limits {
				return errors.New("native phase before limits")
			}
			switch f.Phase {
			case "booting":
				if m.booting || m.started {
					return errors.New("duplicate native booting")
				}
				m.booting = true
			case "running":
				if !m.booting || m.started {
					return errors.New("native running order mismatch")
				}
				m.started = true
				m.running <- struct{}{}
			case "exporting":
				if !m.started {
					return errors.New("native export before running")
				}
			default:
				featureIndex, featureErr := strconv.Atoi(strings.TrimPrefix(f.Phase, "feature:"))
				if !m.started || !strings.HasPrefix(f.Phase, "feature:") || featureErr != nil || featureIndex < 0 {
					return errors.New("invalid native phase")
				}
			}
		case "result":
			if !m.limits || !m.started {
				return errors.New("native result before running")
			}
			m.terminal = true
		case "refused":
			m.terminal = true
		default:
			return errors.New("unknown native frame")
		}
	}
}

// Deno deliberately cannot open protected /sys paths. The paired parent reads
// only these fixed cgroup files and publishes measured controls through RO input.
func publishNativeControls(ctx context.Context, name, inputDir, requestID string) error {
	for {
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", `{{.State.Running}} {{index .Config.Labels "io.deltatronix.shape-owner"}}`, name).Output()
		if err != nil {
			return fmt.Errorf("inspect native control owner: %w", err)
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 || fields[1] != nativeContainerOwner(name) {
			return errors.New("native control owner mismatch")
		}
		if fields[0] == "true" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	cmd := exec.CommandContext(readCtx, "docker", "exec", name, "/bin/cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory.swap.max", "/sys/fs/cgroup/pids.max", "/sys/fs/cgroup/cpu.max")
	out := &boundedNativeBuffer{max: 4096, overflow: readCancel}
	stderr := &boundedNativeBuffer{max: 4096, overflow: readCancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("read native cgroup controls: %w", err)
	}
	controls, err := parseNativeCgroupControls(out.Bytes(), requestID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(controls)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(inputDir, ".controls-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	chmodErr := file.Chmod(0644)
	closeErr := file.Close()
	if err := errors.Join(writeErr, chmodErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(file.Name(), filepath.Join(inputDir, "controls.json"))
}
func parseNativeCgroupControls(data []byte, requestID string) (map[string]any, error) {
	fields := strings.Fields(string(data))
	if len(fields) != 5 {
		return nil, errors.New("invalid native cgroup evidence")
	}
	values := make([]int64, 5)
	for i, field := range fields {
		v, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return nil, errors.New("unbounded native cgroup control")
		}
		values[i] = v
	}
	if values[0] != 1<<30 || values[1] != 0 || values[2] != 64 || values[3] <= 0 || values[4] <= 0 || math.Abs(float64(values[3])/float64(values[4])-1) > 1e-6 {
		return nil, errors.New("native cgroup measurements differ from server policy")
	}
	return map[string]any{"contractVersion": 1, "requestId": requestID, "memoryBytes": values[0], "swapBytes": values[1], "pids": values[2], "cpuQuota": float64(values[3]) / float64(values[4])}, nil
}

func nativeAppliedValid(evidence map[string]any) bool {
	return evidence != nil && evidence["memoryBytes"] == float64(1<<30) && evidence["swapBytes"] == float64(0) && evidence["pids"] == float64(64) && evidence["cpuQuota"] == float64(1) && evidence["runtime"] == "deno" && evidence["runtimeVersion"] == "2.9.8"
}
