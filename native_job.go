package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

type nativeFile struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}
type nativePayload struct {
	ContractVersion int                    `json:"contractVersion"`
	TargetKind      string                 `json:"targetKind"`
	SourceDocuments []nativeSourceDocument `json:"sourceDocuments"`
	AssemblyLimits  map[string]float64     `json:"assemblyLimits"`
	ClosureHash     string                 `json:"closureHash"`
	RequestID       string                 `json:"requestId"`
	DocumentID      string                 `json:"documentId"`
	Version         int                    `json:"version"`
	SnapshotID      string                 `json:"snapshotId"`
	Document        nativeFile             `json:"document"`
	Assets          []nativeFile           `json:"assets"`
	ConfigurationID *string                `json:"configurationId"`
	Operation       json.RawMessage        `json:"operation"`
	Limits          map[string]float64     `json:"limits"`
	Lease           struct {
		ID        string `json:"id"`
		ExpiresAt int64  `json:"expiresAt"`
	} `json:"lease"`
	Runtime struct {
		ArtifactID  string `json:"artifactId"`
		ManifestURL string `json:"manifestUrl"`
	} `json:"runtime"`
}

var nativeHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var nativeArtifactPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var nativePolicy = map[string]float64{"memoryBytes": 1 << 30, "cpuQuota": 1, "pids": 64, "startupMs": 30000, "wallMs": 60000, "cancelGraceMs": 250, "maxDocumentBytes": 4 << 20, "maxAssetBytes": 32 << 20, "maxInputBytes": 64 << 20, "maxFeatures": 256, "maxWorkUnits": 250000, "maxBodies": 256, "maxTriangles": 250000, "maxOutputBytes": 8 << 20, "maxStdoutBytes": 12 << 20, "maxStderrBytes": 64 << 10}

func validateNativePayload(p *nativePayload) error {
	if p == nil || (p.ContractVersion != 1 && p.ContractVersion != 2) || p.RequestID == "" || p.DocumentID == "" || p.Lease.ID == "" || !nativeArtifactPattern.MatchString(p.Runtime.ArtifactID) {
		return errors.New("invalid native assignment identity")
	}
	if len(p.Limits) != len(nativePolicy) {
		return errors.New("native limits missing or unsupported")
	}
	for k, v := range nativePolicy {
		if p.Limits[k] != v {
			return fmt.Errorf("unsupported native policy %s", k)
		}
	}
	expiry := time.UnixMilli(p.Lease.ExpiresAt)
	if !expiry.After(time.Now()) {
		return errors.New("expired native lease")
	}
	if !nativeHashPattern.MatchString(p.Document.Hash) || p.Document.Size <= 0 || p.Document.Size > 4<<20 {
		return errors.New("invalid native document")
	}
	total := p.Document.Size
	seen := map[string]bool{}
	for _, f := range p.Assets {
		if !nativeHashPattern.MatchString(f.Hash) || f.Size <= 0 || f.Size > 32<<20 || seen[f.Hash] {
			return errors.New("invalid native asset")
		}
		seen[f.Hash] = true
		total += f.Size
	}
	if total > 64<<20 {
		return errors.New("native input exceeds limit")
	}
	if p.ContractVersion == 2 {
		return validateNativeAssembly(p, total)
	}
	if p.TargetKind != "" || p.SourceDocuments != nil || p.AssemblyLimits != nil || p.ClosureHash != "" {
		return errors.New("assembly fields require native job version2")
	}
	return nil
}

func fetchNativeInput(ctx context.Context, f nativeFile, dest string) error {
	data, err := nativeFetch(ctx, f.URL, "", f.Size)
	if err != nil {
		return err
	}
	if int64(len(data)) != f.Size || hashNative(data) != f.Hash {
		return errors.New("native input hash/size mismatch")
	}
	return os.WriteFile(dest, data, 0644)
}

type nativeManifestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type nativeManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	ArtifactID    string             `json:"artifactId"`
	Platform      string             `json:"platform"`
	Executable    nativeManifestFile `json:"executable"`
	Wasm          nativeManifestFile `json:"wasm"`
	Archive       struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256"`
		Size   int64  `json:"size"`
	} `json:"archive"`
}

func installNativeRuntime(ctx context.Context, cfg Config, p *nativePayload, dest string) error {
	target := nativeAPIURL(cfg, "/compute/shape-runtime/"+p.Runtime.ArtifactID+"/manifest")
	if p.Runtime.ManifestURL != "" && p.Runtime.ManifestURL != target && p.Runtime.ManifestURL != "/compute/shape-runtime/"+p.Runtime.ArtifactID+"/manifest" {
		return errors.New("untrusted runtime manifest URL")
	}
	data, err := nativeFetch(ctx, target, cfg.AgentToken, 64<<10)
	if err != nil {
		return err
	}
	if _, err := verifyNativeManifest(data); err != nil {
		return err
	}
	var m nativeManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	if m.SchemaVersion != 1 || m.ArtifactID != p.Runtime.ArtifactID || m.Platform != "linux-arm64" || m.Executable.Name != "shape-native" || m.Wasm.Name != "replicad_single.wasm" || m.Archive.Size <= 0 || m.Archive.Size > 128<<20 || !nativeHashPattern.MatchString(m.Archive.SHA256) {
		return errors.New("invalid native runtime manifest")
	}
	archiveURL := nativeAPIURL(cfg, "/compute/shape-runtime/"+p.Runtime.ArtifactID+"/archive")
	if m.Archive.URL != archiveURL && m.Archive.URL != "/compute/shape-runtime/"+p.Runtime.ArtifactID+"/archive" {
		return errors.New("untrusted runtime archive URL")
	}
	archive, err := nativeFetch(ctx, archiveURL, cfg.AgentToken, m.Archive.Size)
	if err != nil {
		return err
	}
	if int64(len(archive)) != m.Archive.Size || hashNative(archive) != m.Archive.SHA256 {
		return errors.New("native archive hash/size mismatch")
	}
	if err := unpackNativeArchive(archive, dest); err != nil {
		return err
	}
	for _, f := range []nativeManifestFile{m.Executable, m.Wasm} {
		if f.Size <= 0 || !nativeHashPattern.MatchString(f.SHA256) {
			return errors.New("invalid native artifact file")
		}
		b, err := os.ReadFile(filepath.Join(dest, f.Name))
		if err != nil {
			return err
		}
		if int64(len(b)) != f.Size || hashNative(b) != f.SHA256 {
			return errors.New("native runtime file hash/size mismatch")
		}
	}
	return nil
}

func nativeJSONRequest(ctx context.Context, cfg Config, path, lease string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, nativeAPIURL(cfg, path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AgentToken)
	req.Header.Set("X-Shape-Lease", lease)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("native API status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

func nativeChildManifest(p *nativePayload) map[string]any {
	assets := make([]map[string]any, 0, len(p.Assets))
	for _, file := range p.Assets {
		assets = append(assets, map[string]any{"hash": file.Hash, "size": file.Size})
	}
	manifest := map[string]any{"contractVersion": p.ContractVersion, "identity": map[string]any{"requestId": p.RequestID, "documentId": p.DocumentID, "version": p.Version}, "documentHash": p.Document.Hash, "configurationId": p.ConfigurationID, "operation": p.Operation, "limits": p.Limits, "assets": assets}
	if p.ContractVersion == 2 {
		sources := make([]map[string]any, 0, len(p.SourceDocuments))
		for _, source := range p.SourceDocuments {
			sources = append(sources, nativeSourceMetadata(source))
		}
		manifest["targetKind"] = p.TargetKind
		manifest["assemblyLimits"] = p.AssemblyLimits
		manifest["sourceDocuments"] = sources
		manifest["closureHash"] = p.ClosureHash
	}
	return manifest
}

func (a *agent) runNativeJob(jobID string, p *nativePayload) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		leaseID := ""
		if p != nil {
			leaseID = p.Lease.ID
		}
		a.send(map[string]any{"type": "job.result", "jobId": jobID, "leaseId": leaseID, "ok": false, "error": "agent busy: one job at a time"})
		return
	}
	a.busy = true
	ctx, cancel := context.WithCancel(a.ctx)
	a.jobs[jobID] = cancel
	a.mu.Unlock()
	cleanupVerified := true
	var finalFrame map[string]any
	defer func() {
		interrupted := ctx.Err() != nil
		cancel()
		a.mu.Lock()
		delete(a.jobs, jobID)
		delete(a.nativeLeases, jobID)
		a.busy = false
		if !cleanupVerified {
			a.nativeReady = false
		}
		a.mu.Unlock()
		if !cleanupVerified {
			a.conn.CloseNow()
			return
		}
		if a.ctx.Err() != nil {
			return
		}
		if interrupted && p != nil {
			finalFrame = map[string]any{"type": "job.result", "jobId": jobID, "leaseId": p.Lease.ID, "ok": false, "error": "Native job cancelled after cleanup."}
		}
		if finalFrame != nil {
			a.send(finalFrame)
		}
	}()
	fail := func(err error) {
		if ctx.Err() == nil {
			finalFrame = map[string]any{"type": "job.result", "jobId": jobID, "leaseId": p.Lease.ID, "ok": false, "error": boundedNativeError(err)}
		}
	}

	if err := validateNativePayload(p); err != nil {
		if p != nil {
			fail(err)
		}
		return
	}
	expiry := time.UnixMilli(p.Lease.ExpiresAt)
	a.mu.Lock()
	if a.nativeLeases == nil {
		a.nativeLeases = map[string]nativeLease{}
	}
	a.nativeLeases[jobID] = nativeLease{p.Lease.ID, expiry}
	a.mu.Unlock()
	a.send(map[string]any{"type": "job.accept", "jobId": jobID, "leaseId": p.Lease.ID})
	go a.renewNativeLease(ctx, jobID, p.Lease.ID, cancel)
	dir, err := os.MkdirTemp("", "dtx-native-")
	if err != nil {
		fail(err)
		return
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			cleanupVerified = false
		}
	}()
	input := filepath.Join(dir, "input")
	output := filepath.Join(dir, "output")
	runtimeDir := filepath.Join(dir, "runtime")
	for _, path := range []string{input, output, runtimeDir, filepath.Join(input, "assets"), filepath.Join(input, "sources")} {
		if err := os.MkdirAll(path, 0755); err != nil {
			fail(err)
			return
		}
		if err := os.Chmod(path, 0755); err != nil {
			fail(err)
			return
		}
	}
	// Only the unprivileged child can write its two explicitly permitted files.
	if err := os.Chmod(output, 0777); err != nil {
		fail(err)
		return
	}
	startupCtx, startupCancel := context.WithTimeout(ctx, 30*time.Second)
	defer startupCancel()
	if err := installNativeRuntime(startupCtx, a.cfg, p, runtimeDir); err != nil {
		fail(err)
		return
	}
	if err := fetchNativeInput(startupCtx, p.Document, filepath.Join(input, "document.json")); err != nil {
		fail(err)
		return
	}
	for _, file := range p.Assets {
		if err := fetchNativeInput(startupCtx, file, filepath.Join(input, "assets", file.Hash)); err != nil {
			fail(err)
			return
		}
	}
	if err := stageNativeSources(startupCtx, p, input); err != nil {
		fail(err)
		return
	}
	job := nativeChildManifest(p)
	data, _ := json.Marshal(job)
	if err := os.WriteFile(filepath.Join(input, "job.json"), data, 0644); err != nil {
		fail(err)
		return
	}
	if err := startupCtx.Err(); err != nil {
		fail(err)
		return
	}
	startupCancel()
	stdout, stderr, receipt, err := runNativeContainer(ctx, "dtx-shape-"+hashNative([]byte(a.cfg.AgentID))[:12]+"-"+hashNative([]byte(jobID))[:12], runtimeDir, input, output, p.RequestID, p.ContractVersion)
	cleanupVerified = receipt != nil && receipt["cleanupVerified"] == true
	if err != nil {
		fail(err)
		return
	}
	receipt["stdoutBytes"] = len(stdout)
	receipt["stderrBytes"] = len(stderr)
	result, err := verifyNativeOutput(stdout, p, output)
	if err != nil {
		fail(err)
		return
	}
	if ctx.Err() != nil {
		return
	}
	var outputReceipt any
	if file, ok := result["file"].(map[string]any); ok && file != nil {
		receipt, err := a.uploadNativeOutput(ctx, jobID, p.Lease.ID, output, file)
		if err != nil {
			fail(err)
			return
		}
		outputReceipt = receipt
	}
	a.mu.Lock()
	lease := a.nativeLeases[jobID]
	valid := lease.ID == p.Lease.ID && time.Now().Before(lease.ExpiresAt)
	a.mu.Unlock()
	if ctx.Err() != nil || !valid {
		return
	}
	frame, err := nativeResultFrame(jobID, p, result, receipt, outputReceipt)
	if err != nil {
		fail(err)
		return
	}

	finalFrame = frame
}

type nativeLease struct {
	ID        string
	ExpiresAt time.Time
}

func (a *agent) renewNativeLease(ctx context.Context, jobID, leaseID string, cancel context.CancelFunc) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	a.mu.Lock()
	lease := a.nativeLeases[jobID]
	a.mu.Unlock()
	expiry := time.NewTimer(time.Until(lease.ExpiresAt))
	defer expiry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.send(map[string]any{"type": "job.renew", "jobId": jobID, "leaseId": leaseID})
		case <-expiry.C:
			a.mu.Lock()
			lease = a.nativeLeases[jobID]
			a.mu.Unlock()
			if lease.ID != leaseID || !time.Now().Before(lease.ExpiresAt) {
				cancel()
				return
			}
			expiry.Reset(time.Until(lease.ExpiresAt))
		}
	}
}

func (a *agent) onNativeLease(jobID, leaseID string, expires int64) {
	expiry := time.UnixMilli(expires)
	a.mu.Lock()
	defer a.mu.Unlock()
	lease := a.nativeLeases[jobID]
	if lease.ID == leaseID && expiry.After(time.Now()) {
		a.nativeLeases[jobID] = nativeLease{leaseID, expiry}
	}
}

func verifyNativeOutput(stdout []byte, p *nativePayload, dir string) (map[string]any, error) {
	raw, err := validateNativeResultLines(stdout)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		ContractVersion int            `json:"contractVersion"`
		RequestID       string         `json:"requestId"`
		Result          map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.ContractVersion != p.ContractVersion || envelope.RequestID != p.RequestID || envelope.Result == nil {
		return nil, errors.New("native result identity mismatch")
	}
	var evidence map[string]any
	for _, line := range bytes.Split(stdout, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var frame struct {
			ContractVersion int            `json:"contractVersion"`
			RequestID       string         `json:"requestId"`
			Type            string         `json:"type"`
			Applied         map[string]any `json:"applied"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			return nil, err
		}
		if frame.ContractVersion != p.ContractVersion || frame.RequestID != p.RequestID {
			return nil, errors.New("native frame identity mismatch")
		}
		if frame.Type == "limits" {
			if evidence != nil {
				return nil, errors.New("duplicate native limits")
			}
			evidence = frame.Applied
		}
	}
	if !nativeAppliedValid(evidence) {
		return nil, errors.New("native applied limits mismatch")
	}

	result := envelope.Result
	for field, max := range map[string]float64{"bodyCount": 256, "triangles": 250000, "work": 250000} {
		if !nativeSummaryCount(result[field], max) {
			return nil, fmt.Errorf("invalid native summary %s", field)
		}
	}
	if p.ContractVersion == 2 {
		if err := validateNativeAssemblySummary(p, result); err != nil {
			return nil, err
		}
	}
	states, ok := result["states"].(map[string]any)
	if !ok || len(states) > 256 {
		return nil, errors.New("invalid native feature states")
	}
	for _, raw := range states {
		state, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("invalid native feature state")
		}
		status := state["status"]
		if status != "ok" && status != "suppressed" && status != "warning" {
			return nil, errors.New("unsuccessful native feature")
		}
	}
	info, err := os.Lstat(filepath.Join(dir, "result.json"))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil, errors.New("invalid native result file")
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil, err
	}
	if result["resultHash"] != hashNative(data) || result["resultSize"] != float64(len(data)) {
		return nil, errors.New("native result hash/size mismatch")
	}
	// Child limits are checked locally; only measured container receipt crosses the wire.
	return result, nil
}

func (a *agent) uploadNativeOutput(ctx context.Context, jobID, leaseID, dir string, file map[string]any) (map[string]any, error) {
	info, err := os.Lstat(filepath.Join(dir, "export.bin"))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil, errors.New("invalid native export file")
	}
	resultInfo, resultErr := os.Lstat(filepath.Join(dir, "result.json"))
	if resultErr != nil || resultInfo.Size()+info.Size() > 8<<20 {
		return nil, errors.New("native aggregate output exceeds limit")
	}
	data, err := os.ReadFile(filepath.Join(dir, "export.bin"))
	if err != nil {
		return nil, err
	}
	if file["hash"] != hashNative(data) || file["size"] != float64(len(data)) {
		return nil, errors.New("native export hash/size mismatch")
	}
	var stage struct {
		FileID      string `json:"fileId"`
		UploadURL   string `json:"uploadUrl"`
		ContentType string `json:"contentType"`
	}
	path := "/compute/shape-jobs/" + jobID
	if err := nativeJSONRequest(ctx, a.cfg, path+"/output-upload", leaseID, file, &stage); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, stage.UploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	expectedMIME := map[string]string{"step": "application/step", "stl": "model/stl"}[fmt.Sprint(file["format"])]
	if expectedMIME == "" || stage.ContentType != expectedMIME {
		return nil, errors.New("native export upload MIME mismatch")
	}
	req.Header.Set("Content-Type", stage.ContentType)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("native upload status %d", resp.StatusCode)
	}
	var receipt map[string]any
	if err := nativeJSONRequest(ctx, a.cfg, path+"/output-complete", leaseID, map[string]any{"fileId": stage.FileID}, &receipt); err != nil {
		return nil, err
	}
	receipt["format"] = file["format"]
	return receipt, nil
}

func boundedNativeError(err error) string {
	s := err.Error()
	if len(s) > 2048 {
		return s[:2048]
	}
	return s
}

func nativeResultFrame(jobID string, p *nativePayload, summary, receipt map[string]any, output any) (map[string]any, error) {
	receipt["artifactId"] = p.Runtime.ArtifactID
	frame := map[string]any{"type": "job.result", "jobId": jobID, "leaseId": p.Lease.ID, "ok": true, "result": map[string]any{"schemaVersion": 1, "summary": summary, "receipt": receipt, "output": output}}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 256<<10 {
		return nil, errors.New("native result envelope exceeds limit")
	}
	return frame, nil
}

func nativeSummaryCount(value any, max float64) bool {
	number, ok := value.(float64)
	return ok && number >= 0 && number <= max && number == float64(int64(number))
}
