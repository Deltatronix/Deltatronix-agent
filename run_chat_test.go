package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket/wsjson"
)

func TestPairedChatToolsRoundTripUsesExplicitModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	const selectedModel = "fixture-chat:7b"
	const messages = `[{"role":"system","content":"Inspect only the selected component."},{"role":"user","content":"Inspect U7 before proposing changes."}]`
	const tools = `[{"type":"function","function":{"name":"inspect_component","description":"Read a component by reference.","strict":true,"parameters":{"type":"object","properties":{"reference":{"type":"string"}},"required":["reference"],"additionalProperties":false}}}]`
	const completion = `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"inspect-u7","type":"function","function":{"name":"inspect_component","arguments":"{\"reference\":\"U7\"}"}}]}}],"usage":{"prompt_tokens":32,"completion_tokens":12,"total_tokens":44}}`
	type modelRequest struct {
		Model     string          `json:"model"`
		Messages  json.RawMessage `json:"messages"`
		Tools     json.RawMessage `json:"tools"`
		Stream    *bool           `json:"stream"`
		MaxTokens int             `json:"max_tokens"`
	}
	var listed, probed, jobSent atomic.Bool
	jobRequests := make(chan modelRequest, 1)
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			listed.Store(true)
			io.WriteString(w, `{"data":[{"id":"fixture-vision:4b"},{"id":"fixture-chat:1b"},{"id":"fixture-chat:7b"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			var request modelRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request); err != nil {
				http.Error(w, "invalid model request", http.StatusBadRequest)
				return
			}
			if jobSent.Load() {
				select {
				case jobRequests <- request:
				default:
					t.Error("one assigned chat job produced more than one model request")
				}
				io.WriteString(w, completion)
				return
			}
			// The model fixture answers the advertised probe tool, without fixing its name.
			var probeTools []struct {
				Function struct{ Name string } `json:"function"`
			}
			if request.Model != selectedModel || !listed.Load() ||
				json.Unmarshal(request.Tools, &probeTools) != nil || len(probeTools) != 1 || probeTools[0].Function.Name == "" {
				t.Error("capability probe must use the explicitly selected installed model and a tool")
				http.Error(w, "invalid tool probe", http.StatusBadRequest)
				return
			}
			probed.Store(true)
			json.NewEncoder(w).Encode(map[string]any{
				"model": selectedModel,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{
						"role": "assistant", "content": nil,
						"tool_calls": []any{map[string]any{
							"id": "probe-ok", "type": "function",
							"function": map[string]any{"name": probeTools[0].Function.Name, "arguments": `{"ok":true}`},
						}},
					},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(modelServer.Close)
	conn := connectPairedChat(t, ctx, modelServer.URL+"/v1", selectedModel, 8192).Conn
	var capabilities struct {
		Type         string   `json:"type"`
		Capabilities []string `json:"capabilities"`
	}
	if err := wsjson.Read(ctx, conn, &capabilities); err != nil {
		t.Fatal(err)
	}
	if capabilities.Type != "capabilities" || !slices.Contains(capabilities.Capabilities, "llm.vision") {
		t.Errorf("existing vision capability missing: %+v", capabilities)
	}
	if !slices.Contains(capabilities.Capabilities, "llm.chat") || !listed.Load() || !probed.Load() {
		t.Error("llm.chat must be advertised after models lookup and a successful selected-model tool probe")
	}

	jobSent.Store(true)
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "job.assign", "jobType": "LLM_CHAT_TOOLS", "jobId": "inspect-u7-job",
		"expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"model": selectedModel, "messages": json.RawMessage(messages), "tools": json.RawMessage(tools), "maxTokens": 128,
		},
	}); err != nil {
		t.Fatal(err)
	}
	assertJSON := func(label string, got json.RawMessage, want string) {
		t.Helper()
		var actual, expected any
		if err := json.Unmarshal([]byte(want), &expected); err != nil {
			t.Fatalf("invalid %s fixture: %v", label, err)
		}
		if err := json.Unmarshal(got, &actual); err != nil || !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s did not preserve the structured contract: got %s", label, got)
		}
	}
	accepted := false
	for {
		var frame struct {
			Type   string          `json:"type"`
			JobID  string          `json:"jobId"`
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
			Error  string          `json:"error"`
		}
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			t.Fatalf("waiting for chat job result: %v", err)
		}
		if frame.JobID != "inspect-u7-job" {
			t.Fatalf("response belongs to a different job: %q", frame.JobID)
		}
		switch frame.Type {
		case "job.accept":
			accepted = true
		case "job.result":
			if !accepted || !frame.OK {
				t.Errorf("chat job must be accepted and succeed: accepted=%v ok=%v error=%q", accepted, frame.OK, frame.Error)
			}
			assertJSON("chat job result", frame.Result, completion)
			select {
			case request := <-jobRequests:
				if request.Model != selectedModel || request.Stream == nil || *request.Stream || request.MaxTokens != 128 {
					t.Errorf("chat request must preserve exact selected model, non-streaming mode and output cap: model=%q max_tokens=%d", request.Model, request.MaxTokens)
				}
				assertJSON("chat messages", request.Messages, messages)
				assertJSON("chat tools", request.Tools, tools)
			default:
				t.Error("assigned chat job never reached the local chat/completions endpoint")
			}
			return
		default:
			t.Fatalf("unexpected chat job frame: %q", frame.Type)
		}
	}
}
