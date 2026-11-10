package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func loadPairedAssemblyFixture(t *testing.T) nativePairedFixture {
	t.Helper()
	dir := os.Getenv("DTX_NATIVE_ASSEMBLY_FIXTURE")
	if dir == "" {
		t.Skip("requires explicit real assembly fixture directory")
	}
	wire, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var child struct {
		ContractVersion int    `json:"contractVersion"`
		TargetKind      string `json:"targetKind"`
		Identity        struct {
			RequestID  string `json:"requestId"`
			DocumentID string `json:"documentId"`
			Version    int    `json:"version"`
		} `json:"identity"`
		DocumentHash    string             `json:"documentHash"`
		ConfigurationID *string            `json:"configurationId"`
		Operation       json.RawMessage    `json:"operation"`
		Limits          map[string]float64 `json:"limits"`
		AssemblyLimits  map[string]float64 `json:"assemblyLimits"`
		ClosureHash     string             `json:"closureHash"`
		SourceDocuments []struct {
			Kind            string  `json:"kind"`
			ID              string  `json:"id"`
			Version         int     `json:"version"`
			SnapshotID      string  `json:"snapshotId"`
			Hash            string  `json:"hash"`
			Size            int64   `json:"size"`
			ConfigurationID *string `json:"configurationId"`
		} `json:"sourceDocuments"`
		Assets []struct {
			Hash string `json:"hash"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(wire, &child); err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile(filepath.Join(dir, "document.json"))
	if err != nil {
		t.Fatal(err)
	}
	if hashNative(document) != child.DocumentHash {
		t.Fatal("actual assembly root fixture has mismatched bytes")
	}
	expectedData, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(expectedData, &expected); err != nil {
		t.Fatal(err)
	}
	p := nativePayload{ContractVersion: child.ContractVersion, TargetKind: child.TargetKind, RequestID: child.Identity.RequestID, DocumentID: child.Identity.DocumentID, Version: child.Identity.Version, SnapshotID: "fixture-root-snapshot", Document: nativeFile{Hash: child.DocumentHash, Size: int64(len(document))}, ConfigurationID: child.ConfigurationID, Operation: child.Operation, Limits: child.Limits, AssemblyLimits: child.AssemblyLimits, ClosureHash: child.ClosureHash, SourceDocuments: []nativeSourceDocument{}, Assets: []nativeFile{}}
	files := map[string][]byte{"/document": document}
	for _, source := range child.SourceDocuments {
		data, err := os.ReadFile(filepath.Join(dir, "sources", source.Hash+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != source.Size || hashNative(data) != source.Hash {
			t.Fatal("actual assembly source fixture has mismatched bytes")
		}
		files["/sources/"+source.Hash] = data
		p.SourceDocuments = append(p.SourceDocuments, nativeSourceDocument{Kind: source.Kind, ID: source.ID, Version: source.Version, SnapshotID: source.SnapshotID, ConfigurationID: source.ConfigurationID, Document: nativeFile{Hash: source.Hash, Size: source.Size}})
	}
	for _, asset := range child.Assets {
		data, err := os.ReadFile(filepath.Join(dir, "assets", asset.Hash))
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(data)) != asset.Size || hashNative(data) != asset.Hash {
			t.Fatal("actual assembly asset fixture has mismatched bytes")
		}
		files["/assets/"+asset.Hash] = data
		p.Assets = append(p.Assets, nativeFile{Hash: asset.Hash, Size: asset.Size})
	}
	bom, ok := expected["bom"].(map[string]any)
	if !ok {
		t.Fatal("independent fixture BOM missing")
	}
	keys := make([]string, 0, len(bom))
	for key := range bom {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, map[string]any{"key": key, "quantity": bom[key]})
	}
	// This hand-worked fixture has no suppressed/unresolved inventory sources.
	preimage, err := canonicalNativeJSON(map[string]any{"excluded": 0, "lines": lines, "unresolved": 0})
	if err != nil {
		t.Fatal(err)
	}
	return nativePairedFixture{Payload: p, Files: files, ExpectedAssembly: expected, ExpectedBomHash: hashNative(preimage)}
}

func TestNativeAssemblyActualPairedVerification(t *testing.T) {
	runNativePairedFixture(t, loadPairedAssemblyFixture(t), nativeComplete)
}
func TestNativeAssemblyActualCancelThenNextJob(t *testing.T) {
	runNativePairedFixture(t, loadPairedAssemblyFixture(t), nativeCancelAndContinue)
}
func TestNativeAssemblyActualProcessStop(t *testing.T) {
	runNativePairedFixture(t, loadPairedAssemblyFixture(t), nativeProcessStop)
}
func TestNativeAssemblyActualRejectsChangedSourceBytes(t *testing.T) {
	fixture := loadPairedAssemblyFixture(t)
	for path, data := range fixture.Files {
		if strings.HasPrefix(path, "/sources/") {
			corrupt := append([]byte(nil), data...)
			corrupt[0] ^= 1
			fixture.Files[path] = corrupt
			break
		}
	}
	fixture.ExpectedError = "native input hash/size mismatch"
	runNativePairedFixture(t, fixture, nativeRefuse)
}
func TestNativeAssemblyActualRejectsMissingSourceBytes(t *testing.T) {
	fixture := loadPairedAssemblyFixture(t)
	for path := range fixture.Files {
		if strings.HasPrefix(path, "/sources/") {
			delete(fixture.Files, path)
			break
		}
	}
	fixture.ExpectedError = "native download status 404"
	runNativePairedFixture(t, fixture, nativeRefuse)
}

func TestNativeAssemblyPreparedInputFixture(t *testing.T) {
	fixture := loadPairedAssemblyFixture(t)
	if fixture.ExpectedBomHash != hashNative([]byte(`{"excluded":0,"lines":[{"key":"shape:solid-source","quantity":12}],"unresolved":0}`)) {
		t.Fatal("independent BOM preimage differs from hand-worked contract")
	}
	p := fixture.Payload
	p.Lease.ID = "fixture-lease"
	p.Lease.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	p.Runtime.ArtifactID = "shape-native-v2-fixture"
	if err := validateNativePayload(&p); err != nil {
		t.Fatal(err)
	}
	wire, err := os.ReadFile(filepath.Join(os.Getenv("DTX_NATIVE_ASSEMBLY_FIXTURE"), "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(wire, &expected); err != nil {
		t.Fatal(err)
	}
	actual, _ := canonicalNativeJSON(nativeChildManifest(&p))
	want, _ := canonicalNativeJSON(expected)
	if string(actual) != string(want) {
		t.Fatalf("prepared real fixture child metadata drifted\nwant %s\ngot %s", want, actual)
	}
}

func TestNativeAssemblyActualDenoGeometry(t *testing.T) {
	runtimeDir := os.Getenv("DTX_NATIVE_RUNTIME")
	if runtimeDir == "" {
		t.Skip("requires explicit compiled native runtime")
	}
	requireNativeRuntimeRelease(t, runtimeDir)
	fixture := loadPairedAssemblyFixture(t)
	p := fixture.Payload
	input := filepath.Join(t.TempDir(), "input")
	output := filepath.Join(t.TempDir(), "output")
	for _, dir := range []string{input, filepath.Join(input, "sources"), filepath.Join(input, "assets"), output} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	os.Chmod(output, 0777)
	for path, data := range fixture.Files {
		dest := filepath.Join(input, strings.TrimPrefix(path, "/"))
		if path == "/document" {
			dest = filepath.Join(input, "document.json")
		} else if strings.HasPrefix(path, "/sources/") {
			dest += ".json"
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(nativeChildManifest(&p))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "job.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, receipt, err := runNativeContainer(context.Background(), "dtx-assembly-test-direct", runtimeDir, input, output, p.RequestID, 2)
	if err != nil {
		t.Fatalf("actual assembly runtime failed: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	summary, err := verifyNativeOutput(stdout, &p, output)
	if err != nil {
		t.Fatalf("actual assembly output invalid: %v stdout=%s stderr=%s", err, stdout, stderr)
	}
	assembly := summary["assembly"].(map[string]any)
	if assembly["closureHash"] != p.ClosureHash || assembly["occurrenceCount"] != fixture.ExpectedAssembly["occurrenceCount"] || assembly["mateCount"] != fixture.ExpectedAssembly["mateCount"] || assembly["bomHash"] != fixture.ExpectedBomHash {
		t.Fatalf("actual assembly differs from independent expected evidence: %v", summary)
	}
	details, err := os.ReadFile(filepath.Join(output, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Bodies []struct {
			Volume float64 `json:"volumeMm3"`
		} `json:"bodies"`
	}
	if err := json.Unmarshal(details, &result); err != nil {
		t.Fatal(err)
	}
	volumes := make([]float64, 0, len(result.Bodies))
	for _, body := range result.Bodies {
		volumes = append(volumes, body.Volume)
	}
	sort.Float64s(volumes)
	if len(volumes) != 2 || math.Abs(volumes[0]-2000) > 0.001 || math.Abs(volumes[1]-3000) > 0.001 {
		t.Fatalf("same-byte A/B configurations lost actual geometry identity: %v", volumes)
	}
	t.Logf("actual Deno assembly: occurrences=%v mates=%v BOM=%v volumes=%v cleanup=%v", assembly["occurrenceCount"], assembly["mateCount"], assembly["bomHash"], volumes, receipt["cleanupVerified"])
}
