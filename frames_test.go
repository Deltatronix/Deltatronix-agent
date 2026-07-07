package main

import (
	"encoding/json"
	"testing"
)

func TestRouteConfigAndCancel(t *testing.T) {
	var cfg Frame
	json.Unmarshal([]byte(`{"type":"config","visionModels":["gemma3:4b"]}`), &cfg)
	if routeFrame(cfg) != kindConfig {
		t.Fatalf("config frame misrouted")
	}
	if len(cfg.VisionModels) != 1 || cfg.VisionModels[0] != "gemma3:4b" {
		t.Fatalf("visionModels decoded wrong: %v", cfg.VisionModels)
	}

	var cancel Frame
	json.Unmarshal([]byte(`{"type":"job.cancel","jobId":"j1"}`), &cancel)
	if routeFrame(cancel) != kindCancel || cancel.JobID != "j1" {
		t.Fatalf("cancel frame misrouted")
	}
}

func TestRouteAssignClean(t *testing.T) {
	raw := `{"type":"job.assign","jobId":"j2","payload":{"model":"gemma3","prompt":"hi","files":[{"url":"http://x/a.webp","mediaType":"image/webp"}],"responseSchema":{"type":"object"}}}`
	var f Frame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if routeFrame(f) != kindAssign {
		t.Fatalf("clean assign misrouted")
	}
	if f.Payload == nil || f.Payload.Model != "gemma3" || len(f.Payload.Files) != 1 {
		t.Fatalf("payload decoded wrong: %+v", f.Payload)
	}
	if f.Payload.Files[0].URL != "http://x/a.webp" {
		t.Fatalf("file url wrong")
	}
}

// The spec lists job.assign fields as { jobId, type, payload } — a duplicate
// `type` key. Go keeps the last, clobbering the envelope type; routeFrame must
// still classify it as an assignment via payload-presence.
func TestRouteAssignDuplicateType(t *testing.T) {
	raw := `{"type":"job.assign","jobId":"j3","type":"LLM_VISION","payload":{"model":"m","prompt":"p","files":[],"responseSchema":{}}}`
	var f Frame
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if f.Type != "LLM_VISION" {
		t.Fatalf("expected duplicate-key to clobber Type to LLM_VISION, got %q", f.Type)
	}
	if routeFrame(f) != kindAssign {
		t.Fatalf("duplicate-type assign misrouted")
	}
}

func TestResultFrame(t *testing.T) {
	ok := resultFrame("j1", json.RawMessage(`{"rows":2}`), "")
	b, _ := json.Marshal(ok)
	if string(b) != `{"jobId":"j1","ok":true,"result":{"rows":2},"type":"job.result"}` {
		t.Fatalf("ok result frame wrong: %s", b)
	}
	fail := resultFrame("j1", nil, "boom")
	b, _ = json.Marshal(fail)
	if string(b) != `{"error":"boom","jobId":"j1","ok":false,"type":"job.result"}` {
		t.Fatalf("fail result frame wrong: %s", b)
	}
}

func TestWSURL(t *testing.T) {
	cases := map[string]string{
		"https://api.deltatronix.io":  "wss://api.deltatronix.io/compute/ws",
		"http://localhost:3000":       "ws://localhost:3000/compute/ws",
		"https://api.deltatronix.io/": "wss://api.deltatronix.io/compute/ws",
	}
	for in, want := range cases {
		if got := wsURL(in); got != want {
			t.Errorf("wsURL(%q) = %q, want %q", in, got, want)
		}
	}
}
