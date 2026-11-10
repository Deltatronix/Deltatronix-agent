package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNativePolicyRejectsMissingOrChangedLimits(t *testing.T) {
	p := nativePayload{ContractVersion: 1, RequestID: "request", DocumentID: "document", Document: nativeFile{Hash: strings.Repeat("a", 64), Size: 1}}
	p.Lease.ID = "lease"
	p.Lease.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	p.Runtime.ArtifactID = "release"
	p.Limits = map[string]float64{}
	for k, v := range nativePolicy {
		p.Limits[k] = v
	}
	if err := validateNativePayload(&p); err != nil {
		t.Fatal(err)
	}
	p.Limits["memoryBytes"]++
	if validateNativePayload(&p) == nil {
		t.Fatal("changed policy accepted")
	}
	delete(p.Limits, "memoryBytes")
	if validateNativePayload(&p) == nil {
		t.Fatal("missing limit accepted")
	}
}
func TestNativeArtifactRejectsUnsignedManifest(t *testing.T) {
	if _, err := verifyNativeManifest([]byte(`{"schemaVersion":1,"signature":{"algorithm":"ed25519","keyId":"shape-native-release-2026-11","valueBase64":"AAAA"}}`)); err == nil {
		t.Fatal("invalid signature accepted")
	}
}
func TestNativeArchiveRejectsTraversalAndSymlinks(t *testing.T) {
	for _, h := range []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0644}, {Name: "shape-native", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}} {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()
		if unpackNativeArchive(b.Bytes(), t.TempDir()) == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
}
func TestNativeFetchBoundsAndDoesNotForwardBearer(t *testing.T) {
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("bearer leaked")
		}
		w.Write([]byte("oversize"))
	}))
	defer sink.Close()
	oldTransport := http.DefaultTransport
	http.DefaultTransport = sink.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer paired" {
			t.Error("missing paired auth")
		}
		http.Redirect(w, r, sink.URL, http.StatusFound)
	}))
	defer origin.Close() // The sink uses a private test CA trusted only for this test.
	if _, err := nativeFetch(context.Background(), origin.URL, "paired", 2); err == nil {
		t.Fatal("oversize/untrusted response accepted")
	}
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("123")) }))
	defer direct.Close()
	if _, err := nativeFetch(context.Background(), direct.URL, "", 2); err == nil {
		t.Fatal("oversize accepted")
	}
}
func TestNativeContainerHardBounds(t *testing.T) {
	args := strings.Join(nativeContainerArgs("name", "/runtime", "/input", "/output"), " ")
	for _, want := range []string{"--network none", "--read-only", "--user 65534:65534", "--cap-drop ALL", "--security-opt no-new-privileges", "--memory 1073741824", "--memory-swap 1073741824", "--cpus 1", "--pids-limit 64", "--entrypoint /runtime/shape-native", nativeImage} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
}
func TestNativeOutputRejectsWrongEvidenceAndChangedBytes(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"bodies":[]}`)
	os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)
	p := &nativePayload{RequestID: "request"}
	applied := map[string]any{"memoryBytes": 1 << 30, "swapBytes": 0, "pids": 64, "cpuQuota": 1, "runtime": "deno", "runtimeVersion": "2.9.8"}
	lines := func() []byte {
		a, _ := json.Marshal(map[string]any{"contractVersion": 1, "requestId": "request", "type": "limits", "applied": applied})
		b, _ := json.Marshal(map[string]any{"contractVersion": 1, "requestId": "request", "type": "result", "result": map[string]any{"bodyCount": 0, "triangles": 0, "work": 0, "states": map[string]any{}, "resultHash": hashNative(data), "resultSize": len(data)}})
		return append(append(a, '\n'), b...)
	}
	if _, err := verifyNativeOutput(lines(), p, dir); err != nil {
		t.Fatal(err)
	}
	applied["runtime"] = "node"
	if _, err := verifyNativeOutput(lines(), p, dir); err == nil {
		t.Fatal("Node evidence accepted")
	}
	applied["runtime"] = "deno"
	os.WriteFile(filepath.Join(dir, "result.json"), []byte("changed"), 0644)
	if _, err := verifyNativeOutput(lines(), p, dir); err == nil {
		t.Fatal("changed result accepted")
	}
}
func TestNativeLeaseIgnoresOtherLeaseAndExpiry(t *testing.T) {
	a := &agent{nativeLeases: map[string]nativeLease{"job": {ID: "current", ExpiresAt: time.Now().Add(time.Second)}}}
	before := a.nativeLeases["job"].ExpiresAt
	a.onNativeLease("job", "old", time.Now().Add(time.Minute).UnixMilli())
	a.onNativeLease("job", "current", time.Now().Add(-time.Minute).UnixMilli())
	if !a.nativeLeases["job"].ExpiresAt.Equal(before) {
		t.Fatal("stale renewal changed lease")
	}
}

// Explicit opt-in integration probe: a hostile child ignores stdin and blocks.
// The normal companion entrypoint remains the signed Deno executable.
func TestNativeDockerBlockingChildKilledAndNextJobRuns(t *testing.T) {
	if os.Getenv("DTX_NATIVE_DOCKER_TEST") != "1" {
		t.Skip("explicit local Docker integration probe")
	}
	root := t.TempDir()
	os.Chmod(root, 0755)
	runtimeDir := filepath.Join(root, "runtime")
	input := filepath.Join(root, "input")
	output := filepath.Join(root, "output")
	for _, dir := range []string{runtimeDir, input, output} {
		os.MkdirAll(dir, 0755)
	}
	binary := filepath.Join(runtimeDir, "shape-native")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(time.Second, cancel)
	start := time.Now()
	_, _, _, err := runNativeContainer(ctx, "dtx-shape-test-blocking", runtimeDir, input, output, "request")
	if err == nil {
		t.Fatal("blocking child succeeded")
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("blocking child was not bounded")
	}
	if out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "name=dtx-shape-test-blocking").Output(); err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("orphan container: %s %v", out, err)
	}
	os.Remove(filepath.Join(input, "controls.json"))
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nwhile [ ! -f /input/controls.json ]; do sleep 0.01; done\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	_, _, receipt, err := runNativeContainer(context.Background(), "dtx-shape-test-next", runtimeDir, input, output, "request")
	if err != nil {
		t.Fatal(err)
	}
	if receipt["cleanupVerified"] != true {
		t.Fatal("cleanup not verified")
	}
}

func TestNativeGenuineDenoRuntime(t *testing.T) {
	runtimeDir := os.Getenv("DTX_NATIVE_RUNTIME")
	documentPath := os.Getenv("DTX_NATIVE_DOCUMENT")
	if runtimeDir == "" || documentPath == "" {
		t.Skip("requires explicit compiled Deno fixture paths")
	}
	data, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	os.Chmod(root, 0755)
	input := filepath.Join(root, "input")
	output := filepath.Join(root, "output")
	os.MkdirAll(input, 0755)
	os.MkdirAll(output, 0777)
	os.Chmod(output, 0777)
	os.WriteFile(filepath.Join(input, "document.json"), data, 0644)
	p := &nativePayload{RequestID: "companion-real-deno"}
	job := map[string]any{"contractVersion": 1, "identity": map[string]any{"requestId": p.RequestID, "documentId": "companion-test", "version": 1}, "documentHash": hashNative(data), "configurationId": nil, "operation": map[string]any{"kind": "regenerate"}, "limits": nativePolicy, "assets": []any{}}
	wire, _ := json.Marshal(job)
	os.WriteFile(filepath.Join(input, "job.json"), wire, 0644)
	stdout, stderr, receipt, err := runNativeContainer(context.Background(), "dtx-shape-test-deno", runtimeDir, input, output, p.RequestID)
	if err != nil {
		t.Fatalf("Deno run: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	summary, err := verifyNativeOutput(stdout, p, output)
	if err != nil {
		t.Fatalf("Deno output: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	t.Logf("genuine Deno summary=%v receipt=%v", summary, receipt)
}

func TestNativeDockerTouchedMemoryOOM(t *testing.T) {
	if os.Getenv("DTX_NATIVE_DOCKER_TEST") != "1" {
		t.Skip("explicit local Docker integration probe")
	}
	root := t.TempDir()
	os.Chmod(root, 0755)
	runtimeDir := filepath.Join(root, "runtime")
	input := filepath.Join(root, "input")
	output := filepath.Join(root, "output")
	for _, dir := range []string{runtimeDir, input, output} {
		os.MkdirAll(dir, 0755)
	}
	// An adversarial test executable touches real pages beyond the cgroup limit.
	// It is deliberately not the production runtime or an execution fallback.
	script := "#!/bin/sh\nexec /usr/local/bin/node --max-old-space-size=4096 -e 'globalThis.pages=Buffer.alloc(1536*1024*1024,1);setInterval(()=>{},1000)'\n"
	if err := os.WriteFile(filepath.Join(runtimeDir, "shape-native"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	name := "dtx-shape-test-oom"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", nativeContainerArgs(name, runtimeDir, input, output)...).CombinedOutput(); err != nil {
		t.Fatalf("create: %s %v", out, err)
	}
	defer removeNativeContainer(name)
	if out, err := exec.CommandContext(ctx, "docker", "start", "--attach", name).CombinedOutput(); err == nil {
		t.Fatalf("unbounded allocation succeeded: %s", out)
	}
	inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer inspectCancel()
	out, err := exec.CommandContext(inspectCtx, "docker", "inspect", "--format", "{{.State.OOMKilled}} {{.State.ExitCode}} {{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.PidsLimit}} {{.HostConfig.NanoCpus}}", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "true 137 1073741824 1073741824 64 1000000000" {
		t.Fatalf("missing actual OOM/limit evidence: %s", out)
	}
	t.Logf("actual touched-memory evidence: %s", out)
}

func TestNativeLeaseExpiresWithoutRenewal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &agent{nativeLeases: map[string]nativeLease{"job": {ID: "lease", ExpiresAt: time.Now().Add(30 * time.Millisecond)}}}
	go a.renewNativeLease(ctx, "job", "lease", cancel)
	select {
	case <-ctx.Done():
	case <-time.After(300 * time.Millisecond):
		t.Fatal("expired lease did not cancel promptly")
	}
}

func TestNativeSignedRuntimeInstall(t *testing.T) {
	dir := os.Getenv("DTX_NATIVE_SIGNED_RUNTIME")
	if dir == "" {
		t.Skip("requires explicit signed release fixture")
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m nativeManifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-paired-token" {
			t.Error("missing paired auth")
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			w.Write(manifest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/archive") {
			http.ServeFile(w, r, filepath.Join(dir, "archive.tgz"))
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	p := &nativePayload{}
	p.Runtime.ArtifactID = m.ArtifactID
	p.Runtime.ManifestURL = "/compute/shape-runtime/" + m.ArtifactID + "/manifest"
	dest := t.TempDir()
	if err := installNativeRuntime(context.Background(), Config{APIURL: server.URL, AgentToken: "test-paired-token"}, p, dest); err != nil {
		t.Fatal(err)
	}
	t.Logf("verified signed archive/executable/WASM: %s", m.ArtifactID)
}

func TestNativeStartupAndWorkDeadlines(t *testing.T) {
	t.Run("startup has its own short bound", func(t *testing.T) {
		done := make(chan error)
		running := make(chan struct{})
		start := time.Now()
		exited, err := waitNativeExecution(context.Background(), done, running, 15*time.Millisecond, time.Second)
		if exited || err == nil || !strings.Contains(err.Error(), "startup") || time.Since(start) > 300*time.Millisecond {
			t.Fatalf("wrong startup bound: %v %v", exited, err)
		}
	})
	t.Run("running starts a full work budget", func(t *testing.T) {
		done := make(chan error, 1)
		running := make(chan struct{}, 1)
		go func() {
			time.Sleep(10 * time.Millisecond)
			running <- struct{}{}
			time.Sleep(35 * time.Millisecond)
			done <- nil
		}()
		exited, err := waitNativeExecution(context.Background(), done, running, 30*time.Millisecond, 80*time.Millisecond)
		if !exited || err != nil {
			t.Fatalf("work incorrectly used startup clock: %v %v", exited, err)
		}
	})
	t.Run("work deadline remains bounded", func(t *testing.T) {
		done := make(chan error)
		running := make(chan struct{}, 1)
		running <- struct{}{}
		exited, err := waitNativeExecution(context.Background(), done, running, time.Second, 10*time.Millisecond)
		if exited || err == nil || !strings.Contains(err.Error(), "work") {
			t.Fatalf("wrong work bound: %v %v", exited, err)
		}
	})
}
func TestNativeStreamingRejectsResultBeforeRunningAndLateFrames(t *testing.T) {
	for _, frames := range [][]string{{`{"type":"result"}`}, {`{"type":"limits"}`, `{"type":"limits"}`}, {`{"type":"limits"}`, `{"type":"phase","phase":"booting"}`, `{"type":"phase","phase":"running"}`, `{"type":"result"}`, `{"type":"phase","phase":"exporting"}`}} {
		m := &nativeOutputMonitor{requestID: "request", running: make(chan struct{}, 1)}
		var err error
		for _, frame := range frames {
			var f map[string]any
			json.Unmarshal([]byte(frame), &f)
			f["contractVersion"] = 1
			f["requestId"] = "request"
			data, _ := json.Marshal(f)
			err = m.write(append(data, '\n'))
			if err != nil {
				break
			}
		}
		if err == nil {
			t.Fatal("out-of-order stream accepted")
		}
	}
}

func TestNativeSuccessReceiptNamesAssignedArtifact(t *testing.T) {
	p := &nativePayload{}
	p.Runtime.ArtifactID = "shape-native-v1-0123456789abcdef0123"
	p.Lease.ID = "lease"
	frame, err := nativeResultFrame("job", p, map[string]any{}, map[string]any{"artifactId": "stale"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := frame["result"].(map[string]any)
	receipt := result["receipt"].(map[string]any)
	if receipt["artifactId"] != p.Runtime.ArtifactID || frame["leaseId"] != "lease" {
		t.Fatal("runtime/lease identity lost")
	}
}

func TestNativeMeasuredCgroupControls(t *testing.T) {
	controls, err := parseNativeCgroupControls([]byte("1073741824\n0\n64\n100000 100000\n"), "request")
	if err != nil || controls["requestId"] != "request" || controls["contractVersion"] != 1 {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	for _, data := range []string{"max 0 64 100000 100000", "1073741824 1 64 100000 100000", "1073741824 0 128 100000 100000", "1073741824 0 64 200000 100000"} {
		if _, err := parseNativeCgroupControls([]byte(data), "request"); err == nil {
			t.Fatalf("bad controls accepted: %s", data)
		}
	}
}

func TestNativeStreamingValidatesLimitsBeforeStartingWork(t *testing.T) {
	running := make(chan struct{}, 1)
	m := &nativeOutputMonitor{requestID: "request", running: running}
	frames := []map[string]any{{"type": "limits", "applied": map[string]any{"memoryBytes": 1 << 30, "swapBytes": 0, "pids": 64, "cpuQuota": 1, "runtime": "deno", "runtimeVersion": "2.9.8"}}, {"type": "phase", "phase": "booting"}, {"type": "phase", "phase": "running"}, {"type": "result"}}
	for _, frame := range frames {
		frame["contractVersion"] = 1
		frame["requestId"] = "request"
		data, _ := json.Marshal(frame)
		if err := m.write(append(data, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-running:
	default:
		t.Fatal("work clock never started")
	}
	bad := &nativeOutputMonitor{requestID: "request", running: make(chan struct{}, 1)}
	data := []byte(`{"contractVersion":1,"requestId":"request","type":"limits","applied":{"runtime":"node"}}` + "\n")
	if bad.write(data) == nil {
		t.Fatal("unmeasured/wrong runtime starts work")
	}
}

func TestNativeJobLaunchJoinWaitsForCleanup(t *testing.T) {
	a := &agent{}
	release := make(chan struct{})
	cleaned := make(chan struct{})
	a.launchJob(func() { <-release; close(cleaned) })
	joined := make(chan struct{})
	go func() { a.jobsWG.Wait(); close(joined) }()
	select {
	case <-joined:
		t.Fatal("joined before job cleanup")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-joined:
		select {
		case <-cleaned:
		default:
			t.Fatal("cleanup incomplete")
		}
	case <-time.After(time.Second):
		t.Fatal("job did not join")
	}
}

func TestNativeProcessStopCleansContainer(t *testing.T) {
	if os.Getenv("DTX_NATIVE_STOP_CHILD") == "1" {
		connectForever(Config{APIURL: os.Getenv("DTX_NATIVE_STOP_API"), AgentID: "native-stop-integration", AgentToken: "test-paired-token"})
		return
	}
	release := os.Getenv("DTX_NATIVE_SIGNED_RUNTIME")
	documentPath := os.Getenv("DTX_NATIVE_DOCUMENT")
	if release == "" || documentPath == "" {
		t.Skip("requires explicit signed runtime and document for process integration")
	}
	manifest, err := os.ReadFile(filepath.Join(release, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m nativeManifest
	json.Unmarshal(manifest, &m)
	document, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	jobID := fmt.Sprintf("stop-%d", time.Now().UnixNano())
	requestID := "process-stop-request"
	agentID := "native-stop-integration"
	containerName := "dtx-shape-" + hashNative([]byte(agentID))[:12] + "-" + hashNative([]byte(jobID))[:12]
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/document" {
			w.Write(document)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/manifest") {
			w.Write(manifest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/archive") {
			http.ServeFile(w, r, filepath.Join(release, "archive.tgz"))
			return
		}
		if r.URL.Path != "/compute/ws" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-paired-token" {
			w.WriteHeader(401)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		p := map[string]any{"contractVersion": 1, "requestId": requestID, "documentId": "stop-document", "version": 1, "snapshotId": "stop-snapshot", "document": map[string]any{"hash": hashNative(document), "size": len(document), "url": server.URL + "/document"}, "assets": []any{}, "configurationId": nil, "operation": map[string]any{"kind": "regenerate"}, "limits": nativePolicy, "lease": map[string]any{"id": "stop-lease", "expiresAt": time.Now().Add(10 * time.Second).UnixMilli()}, "runtime": map[string]any{"artifactId": m.ArtifactID, "manifestUrl": "/compute/shape-runtime/" + m.ArtifactID + "/manifest"}}
		frame, _ := json.Marshal(map[string]any{"type": "job.assign", "jobType": "SHAPE_NATIVE", "jobId": jobID, "payload": p})
		if conn.Write(r.Context(), websocket.MessageText, frame) != nil {
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNativeProcessStopCleansContainer$", "-test.v")
	cmd.Env = append(os.Environ(), "DTX_NATIVE_STOP_CHILD=1", "DTX_NATIVE_STOP_API="+server.URL)
	log := &boundedNativeBuffer{max: 64 << 10, overflow: cancel}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { cancel(); _ = cmd.Process.Kill(); _ = removeNativeContainer(containerName) }()
	deadline := time.Now().Add(12 * time.Second)
	running := false
	for time.Now().Before(deadline) {
		out, _ := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", containerName).Output()
		if strings.TrimSpace(string(out)) == "true" {
			running = true
			break
		}
		select {
		case err := <-done:
			t.Fatalf("agent exited before native start: %v %s", err, string(log.Bytes()))
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !running {
		t.Fatalf("native container never started: %s", string(log.Bytes()))
	}
	start := time.Now()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("agent stop: %v %s", err, string(log.Bytes()))
		}
	case <-time.After(8 * time.Second):
		t.Fatal("agent did not finish owned cleanup on interrupt")
	}
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "name="+containerName).Output()
	if err != nil || len(bytes.TrimSpace(out)) != 0 {
		t.Fatalf("process stop left container: %s %v", out, err)
	}
	t.Logf("actual paired process interrupted and container removed in %s", time.Since(start))
}
