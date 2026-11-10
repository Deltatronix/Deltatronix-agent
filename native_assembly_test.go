package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func assemblyWireFixture(t *testing.T) (*nativePayload, map[string]any) {
	t.Helper()
	data, err := os.ReadFile("testdata/assembly-wire-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Assignment struct {
			Payload nativePayload `json:"payload"`
		} `json:"assignment"`
		ChildManifest map[string]any `json:"childManifest"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Assignment.Payload.Lease.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	return &fixture.Assignment.Payload, fixture.ChildManifest
}

func TestNativeAssemblyAssignmentAcceptsAuthoritativeV2Fixture(t *testing.T) {
	p, _ := assemblyWireFixture(t)
	if err := validateNativePayload(p); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAssemblyChildManifestMatchesSharedFixture(t *testing.T) {
	p, expected := assemblyWireFixture(t)
	data, err := json.Marshal(nativeChildManifest(p))
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	json.Unmarshal(data, &actual)
	want, _ := canonicalNativeJSON(expected)
	got, _ := canonicalNativeJSON(actual)
	if string(want) != string(got) {
		t.Fatalf("child wire differs\nwant %s\ngot %s", want, got)
	}
	if strings.Contains(string(data), "https://") || strings.Contains(string(data), `"url"`) {
		t.Fatal("presigned URLs leaked into child manifest")
	}
	digest, err := nativeAssemblyClosureHash(p)
	if err != nil || digest != "ed38d869d6add84b7fdeeae968bbf8aa826c697d8dee8c5b1bf087f6a5a4f272" {
		t.Fatalf("cross-runtime closure hash mismatch: %s %v", digest, err)
	}
}

func TestNativeAssemblyAssignmentRejectsUnboundedAndAmbiguousInputs(t *testing.T) {
	cases := map[string]func(*nativePayload){
		"missing structural policy": func(p *nativePayload) { p.AssemblyLimits = nil },
		"changed structural policy": func(p *nativePayload) { p.AssemblyLimits["maxMates"]++ },
		"unknown structural policy": func(p *nativePayload) { p.AssemblyLimits["callerBudget"] = 1 },
		"wrong target":              func(p *nativePayload) { p.TargetKind = "shape" },
		"wrong operation":           func(p *nativePayload) { p.Operation = json.RawMessage(`{"kind":"export","format":"step"}`) },
		"root configuration":        func(p *nativePayload) { config := "config"; p.ConfigurationID = &config },
		"missing source list":       func(p *nativePayload) { p.SourceDocuments = nil },
		"duplicate source identity": func(p *nativePayload) { p.SourceDocuments = append(p.SourceDocuments, p.SourceDocuments[0]) },
		"unsafe source hash":        func(p *nativePayload) { p.SourceDocuments[0].Document.Hash = "../document" },
		"negative source version":   func(p *nativePayload) { p.SourceDocuments[0].Version = -1 },
		"negative root version":     func(p *nativePayload) { p.Version = -1 },
		"zero source size":          func(p *nativePayload) { p.SourceDocuments[0].Document.Size = 0 },
		"oversized source document": func(p *nativePayload) { p.SourceDocuments[0].Document.Size = (4 << 20) + 1 },
		"conflicting same hash sizes": func(p *nativePayload) {
			other := p.SourceDocuments[0]
			other.ID = "other"
			other.Document.Size++
			p.SourceDocuments = append(p.SourceDocuments, other)
		},
		"too many source descriptors": func(p *nativePayload) {
			source := p.SourceDocuments[0]
			p.SourceDocuments = nil
			for i := 0; i < 65; i++ {
				other := source
				other.Version = i
				p.SourceDocuments = append(p.SourceDocuments, other)
			}
		},
		"aggregate source bytes": func(p *nativePayload) {
			source := p.SourceDocuments[0]
			p.SourceDocuments = nil
			for i := 0; i < 17; i++ {
				other := source
				other.Version = i
				other.Document.Hash = fmt.Sprintf("%064x", i+1)
				other.Document.Size = 4 << 20
				p.SourceDocuments = append(p.SourceDocuments, other)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := assemblyWireFixture(t)
			mutate(p)
			p.ClosureHash, _ = nativeAssemblyClosureHash(p)
			if validateNativePayload(p) == nil {
				t.Fatal("invalid assembly assignment accepted")
			}
		})
	}
	p, _ := assemblyWireFixture(t)
	p.ClosureHash = strings.Repeat("f", 64)
	if validateNativePayload(p) == nil {
		t.Fatal("wrong closure digest accepted")
	}
	p, _ = assemblyWireFixture(t)
	p.ContractVersion = 1
	if validateNativePayload(p) == nil {
		t.Fatal("v1 accepted assembly metadata")
	}
}

func TestNativeAssemblyBudgetCountsUniqueSourceBytesAndAcceptsVersionZero(t *testing.T) {
	p, _ := assemblyWireFixture(t)
	p.Version = 0
	source := p.SourceDocuments[0]
	p.SourceDocuments = nil
	for i := 0; i < 64; i++ {
		other := source
		other.Version = i
		other.Document.Size = 4 << 20
		p.SourceDocuments = append(p.SourceDocuments, other)
	}
	p.ClosureHash, _ = nativeAssemblyClosureHash(p)
	if err := validateNativePayload(p); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAssemblyClosureSortsPinsAndExcludesTransport(t *testing.T) {
	p, _ := assemblyWireFixture(t)
	second := p.SourceDocuments[0]
	second.Version = 10
	p.SourceDocuments = append(p.SourceDocuments, second)
	digest, _ := nativeAssemblyClosureHash(p)
	p.SourceDocuments[0], p.SourceDocuments[1] = p.SourceDocuments[1], p.SourceDocuments[0]
	p.SourceDocuments[0].Document.URL = "https://different.invalid/signed"
	p.RequestID = "another-request"
	p.Runtime.ArtifactID = "updated-runtime"
	other, _ := nativeAssemblyClosureHash(p)
	if digest != other {
		t.Fatal("transport/source ordering changed closure identity")
	}
	configuration := "another-config"
	p.SourceDocuments[0].ConfigurationID = &configuration
	other, _ = nativeAssemblyClosureHash(p)
	if digest == other {
		t.Fatal("configuration identity was omitted")
	}
}

func TestNativeAssemblySourceStagingVerifiesOncePerContentHash(t *testing.T) {
	data := []byte(`{"source":"historical"}`)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("paired token sent to source storage")
		}
		w.Write(data)
	}))
	defer server.Close()
	p, _ := assemblyWireFixture(t)
	p.SourceDocuments[0].Document = nativeFile{Hash: hashNative(data), Size: int64(len(data)), URL: server.URL}
	other := p.SourceDocuments[0]
	other.Version = 0
	p.SourceDocuments = append(p.SourceDocuments, other)
	p.ClosureHash, _ = nativeAssemblyClosureHash(p)
	if err := validateNativePayload(p); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sources"), 0755)
	if err := stageNativeSources(context.Background(), p, dir); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("same source bytes fetched repeatedly")
	}
	stored, err := os.ReadFile(filepath.Join(dir, "sources", hashNative(data)+".json"))
	if err != nil || !bytes.Equal(stored, data) {
		t.Fatal("fixed source staging differs")
	}
	for _, bad := range []nativeFile{{Hash: strings.Repeat("a", 64), Size: int64(len(data)), URL: server.URL}, {Hash: hashNative(data), Size: int64(len(data)) - 1, URL: server.URL}} {
		p.SourceDocuments = []nativeSourceDocument{{Document: bad}}
		if stageNativeSources(context.Background(), p, dir) == nil {
			t.Fatal("corrupt or oversized source accepted")
		}
	}
}

func assemblySummaryFixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/assembly-wire-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Result struct {
			Result struct {
				Summary map[string]any `json:"summary"`
			} `json:"result"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture.Result.Result.Summary
}

func TestNativeAssemblyOutputRequiresExactPinnedCompactSummary(t *testing.T) {
	p, _ := assemblyWireFixture(t)
	summary := assemblySummaryFixture(t)
	dir := t.TempDir()
	details := []byte(`{"verification":"ephemeral"}`)
	os.WriteFile(filepath.Join(dir, "result.json"), details, 0644)
	summary["resultHash"] = hashNative(details)
	summary["resultSize"] = float64(len(details))
	output := func(summary map[string]any, version int) []byte {
		frames := []map[string]any{{"type": "limits", "applied": map[string]any{"memoryBytes": 1 << 30, "swapBytes": 0, "pids": 64, "cpuQuota": 1, "runtime": "deno", "runtimeVersion": "2.9.8"}}, {"type": "phase", "phase": "booting"}, {"type": "phase", "phase": "running"}, {"type": "result", "result": summary}}
		var out bytes.Buffer
		for _, frame := range frames {
			frame["contractVersion"] = version
			frame["requestId"] = p.RequestID
			data, _ := json.Marshal(frame)
			out.Write(data)
			out.WriteByte('\n')
		}
		return out.Bytes()
	}
	if _, err := verifyNativeOutput(output(summary, 2), p, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyNativeOutput(output(summary, 1), p, dir); err == nil {
		t.Fatal("assembly accepted v1 output")
	}
	cases := map[string]func(map[string]any){
		"changed closure":         func(s map[string]any) { s["assembly"].(map[string]any)["closureHash"] = strings.Repeat("a", 64) },
		"occurrence cap":          func(s map[string]any) { s["assembly"].(map[string]any)["occurrenceCount"] = float64(4097) },
		"mate cap":                func(s map[string]any) { s["assembly"].(map[string]any)["mateCount"] = float64(513) },
		"fractional count":        func(s map[string]any) { s["assembly"].(map[string]any)["mateCount"] = 0.5 },
		"bad BOM digest":          func(s map[string]any) { s["assembly"].(map[string]any)["bomHash"] = "bad" },
		"bad verification digest": func(s map[string]any) { s["assembly"].(map[string]any)["verificationHash"] = "bad" },
		"derived poses":           func(s map[string]any) { s["poses"] = []any{} },
		"derived DOF":             func(s map[string]any) { s["assembly"].(map[string]any)["dof"] = float64(0) },
		"export file":             func(s map[string]any) { s["file"] = map[string]any{"format": "step"} },
		"shape states":            func(s map[string]any) { s["states"] = map[string]any{"feature": map[string]any{"status": "ok"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			summary := assemblySummaryFixture(t)
			summary["resultHash"] = hashNative(details)
			summary["resultSize"] = float64(len(details))
			mutate(summary)
			if _, err := verifyNativeOutput(output(summary, 2), p, dir); err == nil {
				t.Fatal("invalid assembly summary accepted")
			}
		})
	}
	frame, err := nativeResultFrame("native-job", p, summary, map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := frame["result"].(map[string]any)
	if result["schemaVersion"] != 1 || result["output"] != nil {
		t.Fatal("result packaging schema drifted from shared contract")
	}
}

func TestNativeAssemblyNontrivialSharedOrdering(t *testing.T) {
	data, err := os.ReadFile("testdata/assembly-ordering-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Payload         nativePayload `json:"payload"`
		ClosurePreimage string        `json:"closurePreimage"`
		ClosureHash     string        `json:"closureHash"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Payload.Lease.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	if err := validateNativePayload(&fixture.Payload); err != nil {
		t.Fatal(err)
	}
	digest, err := nativeAssemblyClosureHash(&fixture.Payload)
	if err != nil || digest != fixture.ClosureHash || hashNative([]byte(fixture.ClosurePreimage)) != digest {
		t.Fatal("binary ordering differs from shared JS vector")
	}
}

func TestNativeAssemblyFixedAssetBoundaryPreservesV1(t *testing.T) {
	p, _ := assemblyWireFixture(t)
	p.Assets = nil
	for i := 0; i < 256; i++ {
		p.Assets = append(p.Assets, nativeFile{Hash: fmt.Sprintf("%064x", i+1), Size: 1})
	}
	p.ClosureHash, _ = nativeAssemblyClosureHash(p)
	if err := validateNativePayload(p); err != nil {
		t.Fatal(err)
	}
	p.Assets = append(p.Assets, nativeFile{Hash: fmt.Sprintf("%064x", 257), Size: 1})
	p.ClosureHash, _ = nativeAssemblyClosureHash(p)
	if validateNativePayload(p) == nil {
		t.Fatal("v2 accepted more than256 distinct closure assets")
	}
	p.ContractVersion = 1
	p.TargetKind = ""
	p.AssemblyLimits = nil
	p.SourceDocuments = nil
	p.ClosureHash = ""
	p.Operation = json.RawMessage(`{"kind":"regenerate"}`)
	if err := validateNativePayload(p); err != nil {
		t.Fatalf("v1 local validation semantics changed: %v", err)
	}
}
