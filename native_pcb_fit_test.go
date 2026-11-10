package main

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func pcbFitWireFixture(t *testing.T) (*nativePayload, map[string]any) {
	t.Helper()
	data, err := os.ReadFile("testdata/pcb-fit-wire-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Assignment struct {
			Payload nativePayload `json:"payload"`
		} `json:"assignment"`
		Child map[string]any `json:"child"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	f.Assignment.Payload.Lease.ExpiresAt = time.Now().Add(time.Minute).UnixMilli()
	return &f.Assignment.Payload, f.Child
}
func TestNativePCBGoldenAssignmentAndChild(t *testing.T) {
	p, child := pcbFitWireFixture(t)
	if err := validateNativePayload(p); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(nativeChildManifest(p))
	var actual map[string]any
	json.Unmarshal(encoded, &actual)
	got, _ := canonicalNativeJSON(actual)
	want, _ := canonicalNativeJSON(child)
	if string(got) != string(want) {
		t.Fatalf("child manifest differs: got %s want %s", got, want)
	}
}

func TestNativePCBRejectsFalseClearReceipt(t *testing.T) {
	p, _ := pcbFitWireFixture(t)
	data, _ := os.ReadFile("testdata/pcb-fit-wire-v3.json")
	var fixture struct {
		Summary map[string]any `json:"summary"`
	}
	json.Unmarshal(data, &fixture)
	dir := t.TempDir()
	details := []byte(`{"ephemeral":true}`)
	os.WriteFile(filepath.Join(dir, "result.json"), details, 0644)
	summary := fixture.Summary
	summary["resultHash"] = hashNative(details)
	summary["resultSize"] = float64(len(details))
	output := func() []byte {
		a, _ := json.Marshal(map[string]any{"contractVersion": 3, "requestId": p.RequestID, "type": "limits", "applied": map[string]any{"memoryBytes": 1 << 30, "swapBytes": 0, "pids": 64, "cpuQuota": 1, "runtime": "deno", "runtimeVersion": "2.9.8"}})
		b, _ := json.Marshal(map[string]any{"contractVersion": 3, "requestId": p.RequestID, "type": "result", "result": summary})
		return append(append(a, '\n'), b...)
	}
	if _, err := verifyNativeOutput(output(), p, dir); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any){
		"unknown coverage":     func(s map[string]any) { s["fit"].(map[string]any)["indeterminateCount"] = float64(1) },
		"no checks":            func(s map[string]any) { s["fit"].(map[string]any)["checkCount"] = float64(0) },
		"wrong proof":          func(s map[string]any) { s["fit"].(map[string]any)["sourceProofHash"] = strings.Repeat("0", 64) },
		"unknown fit field":    func(s map[string]any) { s["fit"].(map[string]any)["geometry"] = []any{} },
		"derived geometry":     func(s map[string]any) { s["bodies"] = []any{} },
		"feature states":       func(s map[string]any) { s["states"] = map[string]any{"feature": map[string]any{"status": "ok"}} },
		"export file":          func(s map[string]any) { s["file"] = map[string]any{"format": "step"} },
		"oversized microstamp": func(s map[string]any) { s["microstamp"] = strings.Repeat("x", 129) },
		"unknown digest":       func(s map[string]any) { s["fit"].(map[string]any)["requirementsHash"] = "invalid" },
		"bad truncation":       func(s map[string]any) { s["fit"].(map[string]any)["moreIssues"] = float64(1) },
	}
	pristine, _ := json.Marshal(summary)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			summary = nil
			json.Unmarshal(pristine, &summary)
			mutate(summary)
			if _, err := verifyNativeOutput(output(), p, dir); err == nil {
				t.Fatal("unsafe summary accepted")
			}
		})
	}
	var examples struct {
		NonClear []map[string]any `json:"nonClearSummaryExamples"`
	}
	json.Unmarshal(data, &examples)
	for _, fit := range examples.NonClear {
		summary = nil
		json.Unmarshal(pristine, &summary)
		summary["fit"] = fit
		if _, err := verifyNativeOutput(output(), p, dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNativePCBRejectsUnboundedCaptureAndConfiguration(t *testing.T) {
	cases := map[string]func(*nativePayload){
		"unsafe request identity": func(p *nativePayload) { p.RequestID = "request/unsafe" },
		"unsafe lease identity":   func(p *nativePayload) { p.Lease.ID = "lease/unsafe" },
		"unknown protocol":        func(p *nativePayload) { p.ContractVersion = 4 },
		"invalid configuration":   func(p *nativePayload) { s := "../escape"; p.ConfigurationID = &s },
		"unsafe version":          func(p *nativePayload) { p.Version = 9007199254740992 },
		"oversized capture": func(p *nativePayload) {
			p.BoardCaptures[0].RawSize = (4 << 20) + 1
			p.Assets[0].Size = p.BoardCaptures[0].RawSize
		},
		"missing asset":         func(p *nativePayload) { p.Assets = nil },
		"wrong hash":            func(p *nativePayload) { p.ProofHash = hashNative([]byte("other")) },
		"duplicate reference":   func(p *nativePayload) { p.BoardCaptures = append(p.BoardCaptures, p.BoardCaptures[0]) },
		"missing fit limits":    func(p *nativePayload) { p.FitLimits = nil },
		"source closure":        func(p *nativePayload) { p.SourceDocuments = []nativeSourceDocument{} },
		"missing captures":      func(p *nativePayload) { p.BoardCaptures = nil },
		"duplicate asset":       func(p *nativePayload) { p.Assets = append(p.Assets, p.Assets[0]) },
		"changed policy":        func(p *nativePayload) { p.FitLimits["maxChecks"]++ },
		"unknown policy":        func(p *nativePayload) { p.FitLimits["futureLimit"] = 1 },
		"unsafe capture hash":   func(p *nativePayload) { p.BoardCaptures[0].Capture.AssetID = "../capture" },
		"capture size mismatch": func(p *nativePayload) { p.BoardCaptures[0].RawSize++ },
		"too many proofs": func(p *nativePayload) {
			for i := 0; i < 256; i++ {
				p.BoardCaptures = append(p.BoardCaptures, p.BoardCaptures[0])
			}
		},
		"input exceeds64MiB": func(p *nativePayload) {
			for i := 0; i < 17; i++ {
				p.Assets = append(p.Assets, nativeFile{Hash: hashNative([]byte(strings.Repeat("a", i+1))), Size: 4 << 20})
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := pcbFitWireFixture(t)
			mutate(p)
			if p.ContractVersion == 3 && name != "wrong hash" {
				p.ProofHash, _ = nativePCBProofHash(p)
			}
			if validateNativePayload(p) == nil {
				t.Fatal("unsafe assignment accepted")
			}
		})
	}
}

func TestNativePCBRejectsUnknownWireFields(t *testing.T) {
	p, _ := pcbFitWireFixture(t)
	data, _ := json.Marshal(nativePayloadWire(t, p))
	var wire map[string]any
	json.Unmarshal(data, &wire)
	wire["boardCaptures"].([]any)[0].(map[string]any)["capture"].(map[string]any)["path"] = "../escape"
	data, _ = json.Marshal(wire)
	var decoded nativePayload
	if json.Unmarshal(data, &decoded) == nil {
		t.Fatal("unknown nested capture field accepted")
	}
}

func TestNativePCBMalformedAssignmentReturnsGenericFailure(t *testing.T) {
	p, _ := pcbFitWireFixture(t)
	payload, _ := json.Marshal(nativePayloadWire(t, p))
	var wire map[string]any
	json.Unmarshal(payload, &wire)
	wire["unknownField"] = "https://secret.invalid/source"
	assignment, _ := json.Marshal(map[string]any{"type": "job.assign", "jobType": "SHAPE_NATIVE", "jobId": "malformed-job", "payload": wire})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		a := &agent{conn: conn, ctx: r.Context(), jobs: map[string]context.CancelFunc{}}
		a.dispatch(assignment)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("malformed authenticated assignment was silently dropped: %v", err)
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["type"] != "job.result" || result["jobId"] != "malformed-job" || result["leaseId"] != p.Lease.ID || result["ok"] != false || result["error"] != "Malformed PCB fit assignment" || result["result"] != nil {
		t.Fatalf("unsafe malformed acknowledgement: %v", result)
	}
}

func TestNativePCBStaleMalformedCannotAcknowledgeActiveLease(t *testing.T) {
	assignment := []byte(`{"type":"job.assign","jobType":"SHAPE_NATIVE","jobId":"current-job","payload":{"contractVersion":3,"lease":{"id":"old-lease"},"unknown":"field"}}`)
	current := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		a := &agent{conn: conn, ctx: r.Context(), busy: true, nativeLeases: map[string]nativeLease{"current-job": {ID: "new-lease", ExpiresAt: time.Now().Add(time.Minute)}}}
		a.dispatch(assignment)
		current <- a.nativeLeases["current-job"].ID
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if _, data, err := conn.Read(ctx); err == nil {
		t.Fatalf("stale malformed assignment emitted terminal acknowledgement: %s", data)
	}
	select {
	case lease := <-current:
		if lease != "new-lease" {
			t.Fatal("stale assignment changed active lease")
		}
	case <-ctx.Done():
		t.Fatal("stale assignment did not recover")
	}
}

func TestNativePCBProofBindsCaptureProvenanceNotTransport(t *testing.T) {
	p, _ := pcbFitWireFixture(t)
	p.RequestID = "another-request"
	p.Document.URL = "https://other.invalid/document"
	p.Assets[0].URL = "https://other.invalid/capture"
	p.Runtime.ArtifactID = "another-signed-artifact"
	if err := validateNativePayload(p); err != nil {
		t.Fatalf("transport/request identity changed proof: %v", err)
	}
	p.BoardCaptures[0].Capture.SourceSnapshotID = "another-source-snapshot"
	if validateNativePayload(p) == nil {
		t.Fatal("changed capture provenance retained previous proof")
	}
}

func TestNativePCBWireRequiresExplicitNullableFields(t *testing.T) {
	data, err := os.ReadFile("testdata/pcb-fit-wire-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"configurationId", "revisionId", "version-null", "legacy-field"} {
		t.Run(field, func(t *testing.T) {
			var fixture map[string]any
			json.Unmarshal(data, &fixture)
			wire := fixture["assignment"].(map[string]any)["payload"].(map[string]any)
			switch field {
			case "configurationId":
				delete(wire, field)
			case "revisionId":
				delete(wire["boardCaptures"].([]any)[0].(map[string]any)["capture"].(map[string]any), field)
			case "version-null":
				wire["version"] = nil
			case "legacy-field":
				wire["sourceDocuments"] = nil
			}
			encoded, _ := json.Marshal(wire)
			var payload nativePayload
			if json.Unmarshal(encoded, &payload) == nil {
				t.Fatal("incomplete or non-V3 wire accepted")
			}
		})
	}
}

// The actual backend emits one protocol's fields, not Go's union zero values.
func nativePayloadWire(t *testing.T, p *nativePayload) map[string]any {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if p.ContractVersion == 3 {
		delete(wire, "sourceDocuments")
		delete(wire, "assemblyLimits")
		delete(wire, "closureHash")
	}
	return wire
}

func TestNativePCBBoardLimitBeforeStaging(t *testing.T) {
	for _, count := range []int{8, 9} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			p, _ := pcbFitWireFixture(t)
			for i := 1; i < count; i++ {
				capture := p.BoardCaptures[0]
				capture.ReferenceID = "pcb-ref-" + strconv.Itoa(i)
				p.BoardCaptures = append(p.BoardCaptures, capture)
			}
			p.ProofHash, _ = nativePCBProofHash(p)
			err := validateNativePayload(p)
			if count == 8 && err != nil {
				t.Fatal(err)
			}
			if count == 9 && err == nil {
				t.Fatal("nine captured boards reached staging despite eight-board policy")
			}
		})
	}
}
