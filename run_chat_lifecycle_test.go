package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type pairedChatFixture struct {
	Conn        *websocket.Conn
	Connections <-chan *websocket.Conn
}

func connectPairedChat(t *testing.T, ctx context.Context, base, model string, contextTokens int) pairedChatFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	previousBase := llmBase
	llmBase = base
	t.Cleanup(func() {
		llmBase = previousBase
	})
	settingsPath, err := llmConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := "backend_url=" + llmBase + "\nchat_model=" + model + "\nchat_context_tokens=" + strconv.Itoa(contextTokens) + "\nlog=off\n"
	if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	connections := make(chan *websocket.Conn, 1)
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/compute/ws" || r.Header.Get("Authorization") != "Bearer synthetic-pair-token" {
			t.Error("compute connection must use the paired bearer authorization and route")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept paired connection: %v", err)
			return
		}
		select {
		case connections <- conn:
		case <-ctx.Done():
			conn.CloseNow()
		}
	}))
	t.Cleanup(backendServer.Close)
	paired := Config{APIURL: backendServer.URL, AgentID: "synthetic-device", AgentToken: "synthetic-pair-token"}
	serveDone := make(chan struct{})
	go func() {
		connectLoop(ctx, paired)
		close(serveDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("paired connection did not stop after cancellation")
		}
	})
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-serveDone:
		t.Fatal("paired connection ended before handshake")
	case <-ctx.Done():
		t.Fatal("timed out waiting for paired connection")
	}
	t.Cleanup(func() { conn.CloseNow() })
	conn.SetReadLimit(64 << 10)
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "config", "visionModels": []string{"fixture-vision:4b"}}); err != nil {
		t.Fatal(err)
	}
	return pairedChatFixture{Conn: conn, Connections: connections}
}

const fixtureChatReply = `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"inspect-u7","type":"function","function":{"name":"inspect_component","arguments":"{\"reference\":\"U7\"}"}}]}}],"usage":{"prompt_tokens":32,"completion_tokens":12,"total_tokens":44}}`

func chatModelServer(t *testing.T, jobHandler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"fixture-vision:4b"},{"id":"fixture-chat:7b"}]}`)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			Tools []struct{ Function struct{ Name string } }
		}
		if json.Unmarshal(body, &request) != nil {
			t.Error("invalid model request")
			return
		}
		if len(request.Tools) == 1 && request.Tools[0].Function.Name == "dtx_probe" {
			io.WriteString(w, `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"probe-ok","type":"function","function":{"name":"dtx_probe","arguments":"{\"ok\":true}"}}]}}]}`)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		jobHandler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type computeReply struct {
	Type         string          `json:"type"`
	JobID        string          `json:"jobId"`
	OK           bool            `json:"ok"`
	Error        string          `json:"error"`
	Result       json.RawMessage `json:"result"`
	Capabilities []string        `json:"capabilities"`
}

func readComputeReply(t *testing.T, ctx context.Context, conn *websocket.Conn) computeReply {
	t.Helper()
	var reply computeReply
	if err := wsjson.Read(ctx, conn, &reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func writeChatFixtureJob(t *testing.T, ctx context.Context, conn *websocket.Conn, id string) {
	t.Helper()
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "job.assign", "jobType": "LLM_CHAT_TOOLS", "jobId": id,
		"expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"model": "fixture-chat:7b", "maxTokens": 128,
			"messages": []any{map[string]any{"role": "user", "content": "Inspect U7."}},
			"tools":    json.RawMessage(`[{"type":"function","function":{"name":"inspect_component","strict":true,"parameters":{"type":"object","properties":{"reference":{"type":"string"}},"required":["reference"],"additionalProperties":false}}}]`),
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPairedChatRejectsDuplicateJSONMembersBeforeInference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var calls atomic.Int32
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, fixtureChatReply) })
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	job := `{"type":"job.assign","jobType":"LLM_CHAT_TOOLS","jobId":"duplicate-json","expiresAt":"` + time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano) + `","payload":{"model":"fixture-chat:7b","model":"fixture-chat:7b","maxTokens":128,"messages":[{"role":"user","content":"Inspect U7."}],"tools":[]}}`
	if err := conn.Write(ctx, websocket.MessageText, []byte(job)); err != nil {
		t.Fatal(err)
	}
	reply := readComputeReply(t, ctx, conn)
	if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "CHAT_INVALID_REQUEST") || calls.Load() != 0 {
		t.Fatalf("duplicate JSON must be rejected before acceptance/inference: type=%s ok=%v code=%q calls=%d", reply.Type, reply.OK, reply.Error, calls.Load())
	}
}

func readChatResult(t *testing.T, ctx context.Context, conn *websocket.Conn, id string) computeReply {
	t.Helper()
	for {
		reply := readComputeReply(t, ctx, conn)
		if reply.JobID != id {
			t.Fatalf("unexpected job id %q", reply.JobID)
		}
		if reply.Type == "job.result" {
			return reply
		}
		if reply.Type != "job.accept" {
			t.Fatalf("unexpected job frame %q", reply.Type)
		}
	}
}

func TestPairedChatRefusesToolArgumentsOutsideAdvertisedSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Replace(fixtureChatReply, `reference`, `unapproved_field`, 1))
	})
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	writeChatFixtureJob(t, ctx, conn, "invalid-tool-args")
	reply := readChatResult(t, ctx, conn, "invalid-tool-args")
	if reply.OK || !strings.HasPrefix(reply.Error, "CHAT_INVALID_TOOL_CALL") || len(reply.Result) != 0 {
		t.Fatalf("tool output outside the advertised schema must not reach the browser as success: ok=%v error=%q", reply.OK, reply.Error)
	}
}

func TestPairedChatDoesNotReexecuteCompletedJobAfterReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	var calls atomic.Int32
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, fixtureChatReply) })
	paired := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192)
	readComputeReply(t, ctx, paired.Conn)
	writeChatFixtureJob(t, ctx, paired.Conn, "replayed-job")
	if reply := readChatResult(t, ctx, paired.Conn, "replayed-job"); !reply.OK {
		t.Fatalf("initial job failed: %s", reply.Error)
	}
	paired.Conn.CloseNow()
	var reconnected *websocket.Conn
	select {
	case reconnected = <-paired.Connections:
	case <-ctx.Done():
		t.Fatal("agent did not reconnect to its paired backend")
	}
	t.Cleanup(func() { reconnected.CloseNow() })
	if err := wsjson.Write(ctx, reconnected, map[string]any{"type": "config", "visionModels": []string{"fixture-vision:4b"}}); err != nil {
		t.Fatal(err)
	}
	readComputeReply(t, ctx, reconnected)
	writeChatFixtureJob(t, ctx, reconnected, "replayed-job")
	reply := readChatResult(t, ctx, reconnected, "replayed-job")
	if reply.OK || !strings.HasPrefix(reply.Error, "CHAT_DUPLICATE_JOB") || calls.Load() != 1 {
		t.Fatalf("reconnect replay must not execute twice: ok=%v code=%q calls=%d", reply.OK, reply.Error, calls.Load())
	}
}

func TestPairedChatCancellationStopsLocalModelRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model, started, stopped := blockingChatModel(t)
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	writeChatFixtureJob(t, ctx, conn, "cancel-chat")
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("local chat request did not start")
	}
	writeChatFixtureJob(t, ctx, conn, "cancel-chat")
	duplicate := readChatResult(t, ctx, conn, "cancel-chat")
	if duplicate.OK || !strings.HasPrefix(duplicate.Error, "CHAT_DUPLICATE_JOB") {
		t.Fatalf("live replay was not refused: code=%q", duplicate.Error)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "job.cancel", "jobId": "cancel-chat"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("job.cancel did not abort the local HTTP request")
	}
	reply := readChatResult(t, ctx, conn, "cancel-chat")
	if reply.OK || !strings.HasPrefix(reply.Error, "CHAT_CANCELLED") || len(reply.Result) != 0 {
		t.Fatalf("cancelled inference must not return success: ok=%v error=%q", reply.OK, reply.Error)
	}
}

func TestPairedChatRejectsExpiredAssignmentBeforeInference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var calls atomic.Int32
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, fixtureChatReply) })
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "job.assign", "jobType": "LLM_CHAT_TOOLS", "jobId": "expired-chat", "expiresAt": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{"model": "fixture-chat:7b", "messages": []any{map[string]any{"role": "user", "content": "Inspect U7."}}, "tools": []any{}, "maxTokens": 128},
	}); err != nil {
		t.Fatal(err)
	}
	reply := readComputeReply(t, ctx, conn)
	if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "CHAT_EXPIRED") || calls.Load() != 0 {
		t.Fatalf("expired job reached acceptance/inference: type=%s error=%q calls=%d", reply.Type, reply.Error, calls.Load())
	}
}

func TestPairedChatRejectsOversizedModelResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat(" ", 64<<10)+fixtureChatReply)
	})
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	writeChatFixtureJob(t, ctx, conn, "oversized-reply")
	reply := readChatResult(t, ctx, conn, "oversized-reply")
	if reply.OK || !strings.HasPrefix(reply.Error, "CHAT_RESPONSE_LIMIT") || len(reply.Result) != 0 {
		t.Fatalf("oversized model output was not refused: ok=%v error=%q", reply.OK, reply.Error)
	}
}

func TestPairedChatDoesNotAdvertiseForTextOnlyModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"fixture-chat:7b"}]}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			calls.Add(1)
			io.WriteString(w, `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"I cannot call tools."}}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(model.Close)
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	capabilities := readComputeReply(t, ctx, conn)
	if slices.Contains(capabilities.Capabilities, "llm.chat") || calls.Load() != 1 {
		t.Fatalf("text-only model must fail the scripted probe: capabilities=%v probes=%d", capabilities.Capabilities, calls.Load())
	}
	writeChatFixtureJob(t, ctx, conn, "text-only")
	reply := readChatResult(t, ctx, conn, "text-only")
	if reply.OK || !strings.HasPrefix(reply.Error, "CHAT_MODEL_UNAVAILABLE") || calls.Load() != 1 {
		t.Fatalf("unverified capability reached inference: code=%q calls=%d", reply.Error, calls.Load())
	}
}

func blockingChatModel(t *testing.T) (*httptest.Server, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	started, stopped := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) != 1 {
			t.Error("live duplicate reexecuted inference")
			http.Error(w, "duplicate", 409)
			return
		}
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	return model, started, stopped
}

func TestPairedChatDisconnectAbortsLocalModelRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model, started, stopped := blockingChatModel(t)
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	writeChatFixtureJob(t, ctx, conn, "disconnect-chat")
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("local inference did not start")
	}
	conn.CloseNow()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("losing the paired connection did not abort local inference")
	}
}

func TestPairedChatRejectsNullJobTypeWithoutVisionFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var calls atomic.Int32
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, fixtureChatReply) })
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "job.assign", "jobType": nil, "jobId": "null-type",
		"payload": map[string]any{"model": "fixture-chat:7b", "messages": []any{map[string]any{"role": "user", "content": "Inspect U7."}}, "tools": []any{}, "maxTokens": 128},
	}); err != nil {
		t.Fatal(err)
	}
	reply := readComputeReply(t, ctx, conn)
	if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "UNSUPPORTED_JOB_TYPE") || calls.Load() != 0 {
		t.Fatalf("explicit null jobType fell back to vision: type=%q error=%q calls=%d", reply.Type, reply.Error, calls.Load())
	}
}
