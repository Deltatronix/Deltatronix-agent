package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadPairedPCBFixture(t *testing.T) nativePairedFixture {
	t.Helper()
	dir := os.Getenv("DTX_NATIVE_PCB_FIXTURE")
	if dir == "" {
		t.Skip("requires explicit genuine PCB fit fixture directory")
	}
	wire, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var child map[string]any
	if err = json.Unmarshal(wire, &child); err != nil {
		t.Fatal(err)
	}
	identity, ok := child["identity"].(map[string]any)
	if !ok {
		t.Fatal("PCB fixture identity missing")
	}
	document, err := os.ReadFile(filepath.Join(dir, "document.json"))
	if err != nil {
		t.Fatal(err)
	}
	if child["documentHash"] != hashNative(document) {
		t.Fatal("PCB fixture root bytes differ from manifest")
	}
	for key, value := range identity {
		child[key] = value
	}
	delete(child, "identity")
	delete(child, "documentHash")
	child["document"] = map[string]any{"hash": hashNative(document), "size": len(document), "url": ""}
	child["snapshotId"] = "fixture-root-snapshot"
	child["lease"] = map[string]any{"id": "fixture-lease", "expiresAt": time.Now().Add(time.Minute).UnixMilli()}
	child["runtime"] = map[string]any{"artifactId": "shape-native-v3-fixture", "manifestUrl": ""}
	for _, raw := range child["assets"].([]any) {
		raw.(map[string]any)["url"] = ""
	}
	payload, err := json.Marshal(child)
	if err != nil {
		t.Fatal(err)
	}
	var p nativePayload
	if err = json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	if err = validateNativePayload(&p); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"/document": document}
	for _, asset := range p.Assets {
		data, err := os.ReadFile(filepath.Join(dir, "assets", asset.Hash))
		if err != nil {
			t.Fatal(err)
		}
		if hashNative(data) != asset.Hash || int64(len(data)) != asset.Size {
			t.Fatal("PCB capture fixture bytes differ from manifest")
		}
		files["/assets/"+asset.Hash] = data
	}
	expectedData, err := os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Fit map[string]any `json:"fit"`
	}
	if err = json.Unmarshal(expectedData, &expected); err != nil {
		t.Fatal(err)
	}
	if len(expected.Fit) == 0 {
		t.Fatal("independent expected PCB fit decision/counts missing")
	}
	return nativePairedFixture{Payload: p, Files: files, ExpectedFit: expected.Fit}
}

func TestNativePCBActualPairedVerification(t *testing.T) {
	runNativePairedFixture(t, loadPairedPCBFixture(t), nativeComplete)
}
func TestNativePCBActualCancelThenNextJob(t *testing.T) {
	runNativePairedFixture(t, loadPairedPCBFixture(t), nativeCancelAndContinue)
}
func TestNativePCBActualProcessStop(t *testing.T) {
	runNativePairedFixture(t, loadPairedPCBFixture(t), nativeProcessStop)
}
func TestNativePCBActualRejectsChangedCaptureBytes(t *testing.T) {
	fixture := loadPairedPCBFixture(t)
	for path, data := range fixture.Files {
		if strings.HasPrefix(path, "/assets/") {
			changed := append([]byte{}, data...)
			changed[0] ^= 1
			fixture.Files[path] = changed
			break
		}
	}
	fixture.ExpectedError = "native input hash/size mismatch"
	runNativePairedFixture(t, fixture, nativeRefuse)
}
func TestNativePCBActualRejectsMissingCaptureBytes(t *testing.T) {
	fixture := loadPairedPCBFixture(t)
	for path := range fixture.Files {
		if strings.HasPrefix(path, "/assets/") {
			delete(fixture.Files, path)
			break
		}
	}
	fixture.ExpectedError = "native download status 404"
	runNativePairedFixture(t, fixture, nativeRefuse)
}
func TestNativePCBActualRejectsUnknownProtocol(t *testing.T) {
	fixture := loadPairedPCBFixture(t)
	fixture.Payload.ContractVersion = 4
	fixture.ExpectedError = "invalid native assignment identity"
	runNativePairedFixture(t, fixture, nativeRefuse)
}

func TestNativePCBActualMalformedThenNextJob(t *testing.T) {
	runNativePairedFixture(t, loadPairedPCBFixture(t), nativeMalformedAndContinue)
}
