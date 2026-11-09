package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// This is a live local-model check, with a synthetic paired backend. It does not
// establish real backend #748 dispatch or an editor inspect/edit/save round trip.
func TestLivePairedLocalChatTools(t *testing.T) {
	base, model, capText := os.Getenv("DTX_LIVE_CHAT_URL"), os.Getenv("DTX_LIVE_CHAT_MODEL"), os.Getenv("DTX_LIVE_CHAT_CONTEXT_TOKENS")
	if base == "" && model == "" && capText == "" {
		t.Skip("opt in with DTX_LIVE_CHAT_URL, DTX_LIVE_CHAT_MODEL and DTX_LIVE_CHAT_CONTEXT_TOKENS")
	}
	cap, err := strconv.Atoi(capText)
	if err != nil || !localChatSettings(LLMSettings{BackendURL: base, ChatModel: model, ChatContextTokens: cap}) {
		t.Fatal("live chat requires an explicit loopback /v1 URL, exact installed model id and positive context cap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	conn := connectPairedChat(t, ctx, base, model, cap).Conn
	capabilities := readComputeReply(t, ctx, conn)
	if capabilities.Type != "capabilities" || !slices.Contains(capabilities.Capabilities, "llm.chat") {
		t.Fatal("selected installed model did not pass the production scripted tool probe")
	}
	job := map[string]any{
		"type": "job.assign", "jobType": "LLM_CHAT_TOOLS", "jobId": "live-tool-roundtrip",
		"expiresAt": time.Now().Add(3 * time.Minute).UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"model": model, "maxTokens": 512,
			"messages": []any{map[string]any{"role": "user", "content": "First call inspect_component exactly once with reference U7. After receiving its tool result, answer with the reference and receipt from that result without further tool calls. This is a read-only synthetic test component."}},
			"tools":    json.RawMessage(`[{"type":"function","function":{"name":"inspect_component","description":"Inspect a synthetic component by reference.","strict":true,"parameters":{"type":"object","properties":{"reference":{"type":"string","const":"U7"}},"required":["reference"],"additionalProperties":false}}}]`),
		},
	}
	if err := wsjson.Write(ctx, conn, job); err != nil {
		t.Fatal(err)
	}
	reply := readChatResult(t, ctx, conn, "live-tool-roundtrip")
	if !reply.OK {
		t.Fatalf("actual selected-model tool job failed: %s", reply.Error)
	}
	originalMessages := job["payload"].(map[string]any)["messages"]
	completeReadOnlyToolFollowup(t, ctx, conn, job, reply, model)

	job["jobId"] = "live-cancel"
	job["expiresAt"] = time.Now().Add(3 * time.Minute).UTC().Format(time.RFC3339Nano)
	job["payload"].(map[string]any)["messages"] = originalMessages
	if err := wsjson.Write(ctx, conn, job); err != nil {
		t.Fatal(err)
	}
	accepted := readComputeReply(t, ctx, conn)
	if accepted.Type != "job.accept" || accepted.JobID != "live-cancel" {
		t.Fatalf("live cancellation job was not admitted: type=%q error=%q", accepted.Type, accepted.Error)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "job.cancel", "jobId": "live-cancel"}); err != nil {
		t.Fatal(err)
	}
	reply = readChatResult(t, ctx, conn, "live-cancel")
	if reply.OK || !strings.HasPrefix(reply.Error, "CHAT_CANCELLED") {
		t.Fatalf("live job cancellation did not terminate the job: ok=%v error=%q", reply.OK, reply.Error)
	}
	// Log metadata only; keep model output, arguments, and pairing credentials out.
	t.Logf("live selected local model passed installation/probe, matching tool-result followup and accepted-job cancellation; explicit context cap=%d", cap)
}

// Shared by the recorded-model control and the opt-in live public-handler test.
func completeReadOnlyToolFollowup(t *testing.T, ctx context.Context, conn *websocket.Conn, job map[string]any, first computeReply, model string) {
	t.Helper()
	var completion struct {
		Model   string
		Choices []struct{ Message json.RawMessage }
	}
	if !first.OK || json.Unmarshal(first.Result, &completion) != nil || completion.Model != model || len(completion.Choices) != 1 {
		t.Fatal("initial completion did not retain the exact selected model")
	}
	var assistant struct {
		Role      string
		ToolCalls []struct {
			ID, Type string
			Function struct{ Name, Arguments string }
		} `json:"tool_calls"`
	}
	if json.Unmarshal(completion.Choices[0].Message, &assistant) != nil || assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatal("expected one assistant tool call")
	}
	call := assistant.ToolCalls[0]
	var args struct{ Reference string }
	if call.ID == "" || call.Type != "function" || call.Function.Name != "inspect_component" || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args.Reference != "U7" {
		t.Fatal("initial completion did not emit the scripted read-only tool call")
	}
	payload := job["payload"].(map[string]any)
	if payload["model"] != model {
		t.Fatal("followup must retain the explicitly selected model")
	}
	messages := append([]any(nil), payload["messages"].([]any)...)
	payload["messages"] = append(messages, json.RawMessage(completion.Choices[0].Message), map[string]any{
		"role": "tool", "tool_call_id": call.ID, "content": `{"reference":"U7","receipt":"synthetic-u7-receipt-42"}`,
	})
	id := job["jobId"].(string) + "-followup"
	job["jobId"] = id
	job["expiresAt"] = time.Now().Add(3 * time.Minute).UTC().Format(time.RFC3339Nano)
	if err := wsjson.Write(ctx, conn, job); err != nil {
		t.Fatal(err)
	}
	reply := readChatResult(t, ctx, conn, id)
	var followup struct {
		Model   string
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role, Content string
				ToolCalls     []json.RawMessage `json:"tool_calls"`
			}
		}
	}
	if !reply.OK || json.Unmarshal(reply.Result, &followup) != nil || followup.Model != model || len(followup.Choices) != 1 {
		t.Fatal("tool-result followup did not complete on the same exact selected model")
	}
	choice := followup.Choices[0]
	if choice.FinishReason != "stop" || choice.Message.Role != "assistant" || len(choice.Message.ToolCalls) != 0 || !strings.Contains(choice.Message.Content, "U7") || !strings.Contains(choice.Message.Content, "synthetic-u7-receipt-42") {
		t.Fatal("followup must finish without another tool call and acknowledge the synthetic tool result")
	}
}
