package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func chatReviewJob(id, schema string) map[string]any {
	return map[string]any{
		"type": "job.assign", "jobType": "LLM_CHAT_TOOLS", "jobId": id,
		"expiresAt": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"model": "fixture-chat:7b", "maxTokens": 128,
			"messages": []any{map[string]any{"role": "user", "content": "Inspect the synthetic component."}},
			"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "inspect_component", "strict": true, "parameters": json.RawMessage(schema)}}},
		},
	}
}

func TestPairedChatRejectsCaseVariantTypedMembers(t *testing.T) {
	const simple = `{"type":"object","properties":{"reference":{"type":"string"}},"required":["reference"],"additionalProperties":false}`
	for _, where := range []string{"schema", "envelope", "payload", "message", "tool", "nested-duplicate", "anyof-schema-alias", "anyof-key-alias"} {
		t.Run(where, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			var calls atomic.Int32
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, fixtureChatReply) })
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			job := chatReviewJob("case-member", simple)
			payload := job["payload"].(map[string]any)
			switch where {
			case "schema":
				job = chatReviewJob("case-member", `{"type":"object","properties":{"value":{"type":"integer","maximum":1,"Maximum":100}},"required":["value"],"additionalProperties":false}`)
			case "anyof-schema-alias":
				job = chatReviewJob("case-member", `{"type":"object","properties":{"value":{"anyOf":[{"type":"integer","maximum":1,"Maximum":100},{"type":"null"}]}},"required":["value"],"additionalProperties":false}`)
			case "anyof-key-alias":
				job = chatReviewJob("case-member", `{"type":"object","properties":{"value":{"AnyOf":[{"type":"integer"},{"type":"null"}]}},"required":["value"],"additionalProperties":false}`)
			case "envelope":
				job["JobType"] = "LLM_VISION"
			case "payload":
				payload["Model"] = "fixture-chat:7b"
			case "message":
				payload["messages"] = []any{map[string]any{"role": "user", "Role": "system", "content": "Inspect U7."}}
			case "tool":
				payload["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["Name"] = "inspect_component"
			case "nested-duplicate":
				job = chatReviewJob("case-member", `{"type":"object","properties":{"nested":{"type":"object","properties":{"value":{"type":"integer","maximum":1,"maximum":100}},"required":["value"],"additionalProperties":false}},"required":["nested"],"additionalProperties":false}`)
			}
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			reply := readComputeReply(t, ctx, conn)
			if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "CHAT_INVALID_REQUEST") || calls.Load() != 0 {
				t.Fatalf("typed member alias must be refused before inference: type=%s error=%q calls=%d", reply.Type, reply.Error, calls.Load())
			}
		})
	}
}

func TestPairedChatPreservesDistinctCaseSensitivePropertyNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"case-sensitive","type":"function","function":{"name":"inspect_component","arguments":"{\"value\":1,\"Value\":2}"}}]}}]}`)
	})
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	job := chatReviewJob("distinct-properties", `{"type":"object","properties":{"value":{"type":"integer","maximum":1},"Value":{"type":"integer","maximum":2}},"required":["value","Value"],"additionalProperties":false}`)
	if err := wsjson.Write(ctx, conn, job); err != nil {
		t.Fatal(err)
	}
	if reply := readChatResult(t, ctx, conn, "distinct-properties"); !reply.OK {
		t.Fatalf("distinct arbitrary property names must remain valid: %q", reply.Error)
	}
}

func readReviewProbeRequest(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	// Read through EOF before blocking so net/http can observe peer disconnects.
	body, err := io.ReadAll(io.LimitReader(r.Body, (64<<10)+1))
	var request struct {
		Model    string
		Messages []struct{ Role, Content string }
		Tools    []struct {
			Type     string
			Function struct {
				Name   string
				Strict bool
			}
		}
		Stream *bool
	}
	if err != nil || len(body) > 64<<10 || json.Unmarshal(body, &request) != nil || request.Model != "fixture-chat:7b" || len(request.Messages) != 1 || request.Messages[0].Role != "user" || request.Messages[0].Content == "" || len(request.Tools) != 1 || request.Tools[0].Type != "function" || request.Tools[0].Function.Name != "dtx_probe" || !request.Tools[0].Function.Strict || request.Stream == nil || *request.Stream {
		t.Error("invalid initial local tool-probe HTTP request")
		http.Error(w, "invalid probe request", http.StatusBadRequest)
		return false
	}
	return true
}

func TestPairedChatInitialProbeDisconnectAbortsHTTP(t *testing.T) {
	for _, termination := range []string{"disconnect", "agent-cancel"} {
		t.Run(termination, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			started, stopped := make(chan struct{}), make(chan struct{})
			escape := make(chan struct{})
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					io.WriteString(w, `{"data":[{"id":"fixture-chat:7b"}]}`)
					return
				}
				if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
					if !readReviewProbeRequest(t, w, r) {
						return
					}
					close(started)
					select {
					case <-r.Context().Done():
						close(stopped)
					case <-escape:
					}
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(func() { close(escape); model.CloseClientConnections(); model.Close() })
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("initial tool probe did not start")
			}
			if termination == "disconnect" {
				conn.CloseNow()
			} else {
				cancel()
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("paired disconnect did not promptly abort the initial probe HTTP request")
			}

		})
	}
}

func writeReviewToolReply(t *testing.T, w io.Writer, name, args string) {
	t.Helper()
	encoded, _ := json.Marshal(args)
	encodedName, _ := json.Marshal(name)
	if _, err := fmt.Fprintf(w, `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"review-call","type":"function","function":{"name":%s,"arguments":%s}}]}}]}`, encodedName, encoded); err != nil {
		t.Error(err)
	}
}

func TestPairedChatEnforcesExactNumericSchemaBounds(t *testing.T) {
	for _, example := range []struct {
		name, property, args string
		ok                   bool
	}{
		{"fraction-is-not-integer", `{"type":"integer"}`, `{"value":1.0000000000000001}`, false},
		{"over-exact-integer-maximum", `{"type":"integer","maximum":9007199254740992}`, `{"value":9007199254740993}`, false},
		{"exact-integer-boundary", `{"type":"integer","maximum":9007199254740992}`, `{"value":9007199254740992}`, true},
		{"ordinary-equivalent-decimal", `{"type":"number","minimum":0.1,"maximum":0.3,"enum":[0.1],"const":0.1}`, `{"value":0.10}`, true},
		{"unsupported-precision", `{"type":"number"}`, `{"value":0.` + strings.Repeat("1", 65) + `}`, false},
	} {
		t.Run(example.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeReviewToolReply(t, w, "inspect_component", example.args)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			job := chatReviewJob("numeric-bounds", `{"type":"object","properties":{"value":`+example.property+`},"required":["value"],"additionalProperties":false}`)
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			reply := readChatResult(t, ctx, conn, "numeric-bounds")
			if reply.OK != example.ok || (!example.ok && !strings.HasPrefix(reply.Error, "CHAT_INVALID_TOOL_CALL")) {
				t.Fatalf("numeric schema conformance: ok=%v want=%v error=%q", reply.OK, example.ok, reply.Error)
			}
		})
	}
}

func TestPairedChatToolResultFollowupKeepsSelectedModel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string
			Messages []struct {
				Role       string
				Content    json.RawMessage
				ToolCalls  []struct{ ID string } `json:"tool_calls"`
				ToolCallID string                `json:"tool_call_id"`
			}
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "fixture-chat:7b" {
			t.Error("followup changed the selected model")
			http.Error(w, "bad request", 400)
			return
		}
		if len(request.Messages) == 1 {
			writeReviewToolReply(t, w, "inspect_component", `{"reference":"U7"}`)
			return
		}
		if len(request.Messages) != 3 || request.Messages[1].Role != "assistant" || len(request.Messages[1].ToolCalls) != 1 || request.Messages[1].ToolCalls[0].ID != "review-call" || request.Messages[2].Role != "tool" || request.Messages[2].ToolCallID != "review-call" {
			t.Error("tool-result transcript lost the actual assistant call ID")
			http.Error(w, "bad transcript", 400)
			return
		}
		var result string
		if json.Unmarshal(request.Messages[2].Content, &result) != nil || result != `{"reference":"U7","receipt":"synthetic-u7-receipt-42"}` {
			t.Error("synthetic tool result was not forwarded")
			http.Error(w, "bad tool result", 400)
			return
		}
		io.WriteString(w, `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"U7 synthetic-u7-receipt-42"}}]}`)
	})
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	readComputeReply(t, ctx, conn)
	job := chatReviewJob("tool-transcript", `{"type":"object","properties":{"reference":{"type":"string"}},"required":["reference"],"additionalProperties":false}`)
	if err := wsjson.Write(ctx, conn, job); err != nil {
		t.Fatal(err)
	}
	first := readChatResult(t, ctx, conn, "tool-transcript")
	completeReadOnlyToolFollowup(t, ctx, conn, job, first, "fixture-chat:7b")
}

func TestPairedChatTransportsSharedPcbNullableFields(t *testing.T) {
	// Original synthetic wire fixtures use the first-party component-search and
	// symbol-field contracts. The browser retains normalization and permissions.
	for _, example := range []struct{ name, schema, args string }{
		{"pcb_components_search", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":160},"limit":{"type":["integer","null"],"minimum":1,"maximum":20}},"required":["query","limit"],"additionalProperties":false}`, `{"query":"Synthetic PX7","limit":null}`},
		{"pcb_schematic_set_symbol_field", `{"type":"object","properties":{"reference":{"type":"string","minLength":1,"maxLength":128},"sheet":{"type":["string","null"],"minLength":1,"maxLength":1024},"field":{"type":"string","enum":["Value"]},"value":{"type":"string","maxLength":1024}},"required":["reference","sheet","field","value"],"additionalProperties":false}`, `{"reference":"R7","sheet":null,"field":"Value","value":"22k"}`},
		{"pcb_components_search", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":160},"limit":{"anyOf":[{"type":"integer","minimum":1,"maximum":20},{"type":"null"}]}},"required":["query","limit"],"additionalProperties":false}`, `{"query":"Synthetic PX7","limit":null}`},
		{"pcb_schematic_set_symbol_field", `{"type":"object","properties":{"reference":{"type":"string","minLength":1,"maxLength":128},"sheet":{"anyOf":[{"type":"null"},{"type":"string","minLength":1,"maxLength":1024}]},"field":{"type":"string","enum":["Value"]},"value":{"type":"string","maxLength":1024}},"required":["reference","sheet","field","value"],"additionalProperties":false}`, `{"reference":"R7","sheet":null,"field":"Value","value":"22k"}`},
	} {
		t.Run(example.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model    string
					Messages []json.RawMessage `json:"messages"`
					Tools    []struct {
						Function struct {
							Name       string
							Parameters json.RawMessage
						}
					}
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "fixture-chat:7b" || len(request.Tools) != 1 || request.Tools[0].Function.Name != example.name {
					t.Error("shared tool request lost its selected model or catalog identity")
					http.Error(w, "invalid request", 400)
					return
				}
				var actual, expected any
				if json.Unmarshal(request.Tools[0].Function.Parameters, &actual) != nil || json.Unmarshal([]byte(example.schema), &expected) != nil || !reflect.DeepEqual(actual, expected) {
					t.Error("companion rewrote the shared strict schema")
					http.Error(w, "changed schema", 400)
					return
				}
				if len(request.Messages) == 1 {
					writeReviewToolReply(t, w, example.name, example.args)
					return
				}
				var assistant struct {
					Role      string
					ToolCalls []struct {
						ID       string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				var result struct {
					Role       string
					ToolCallID string `json:"tool_call_id"`
					Content    string
				}
				if len(request.Messages) != 3 || json.Unmarshal(request.Messages[1], &assistant) != nil || assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "review-call" || assistant.ToolCalls[0].Function.Name != example.name || assistant.ToolCalls[0].Function.Arguments != example.args || json.Unmarshal(request.Messages[2], &result) != nil || result.Role != "tool" || result.ToolCallID != "review-call" || result.Content != `{"status":"synthetic-browser-result","receipt":"nullable-history"}` {
					t.Error("shared tool history changed null arguments or the matching tool result")
					http.Error(w, "invalid history", 400)
					return
				}
				io.WriteString(w, `{"model":"fixture-chat:7b","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"nullable-history"}}]}`)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			job := chatReviewJob("shared-nullable", example.schema)
			job["payload"].(map[string]any)["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] = example.name
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			reply := readChatResult(t, ctx, conn, "shared-nullable")
			var result struct {
				Model   string
				Choices []struct{ Message json.RawMessage }
			}
			var assistant struct {
				Role      string
				ToolCalls []struct {
					ID       string
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
			if !reply.OK || json.Unmarshal(reply.Result, &result) != nil || result.Model != "fixture-chat:7b" || len(result.Choices) != 1 || json.Unmarshal(result.Choices[0].Message, &assistant) != nil || assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
				t.Fatalf("shared nullable tool was unavailable: %q", reply.Error)
			}
			call := assistant.ToolCalls[0]
			if call.ID == "" || call.Function.Name != example.name || call.Function.Arguments != example.args {
				t.Fatal("companion must preserve null omission markers for browser-owned normalization")
			}
			payload := job["payload"].(map[string]any)
			messages := append([]any(nil), payload["messages"].([]any)...)
			payload["messages"] = append(messages, json.RawMessage(result.Choices[0].Message), map[string]any{"role": "tool", "tool_call_id": call.ID, "content": `{"status":"synthetic-browser-result","receipt":"nullable-history"}`})
			job["jobId"] = "shared-nullable-followup"
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			followup := readChatResult(t, ctx, conn, "shared-nullable-followup")
			var completed struct {
				Model   string
				Choices []struct {
					FinishReason string `json:"finish_reason"`
					Message      struct{ Role, Content string }
				}
			}
			if !followup.OK || json.Unmarshal(followup.Result, &completed) != nil || completed.Model != "fixture-chat:7b" || len(completed.Choices) != 1 || completed.Choices[0].FinishReason != "stop" || completed.Choices[0].Message.Role != "assistant" || completed.Choices[0].Message.Content != "nullable-history" {
				t.Fatalf("shared nullable tool history did not complete: %q", followup.Error)
			}

		})
	}
}

func TestPairedChatRejectsUnsupportedTypeUnions(t *testing.T) {
	for _, property := range []string{
		`{"type":["string","integer"]}`,
		`{"type":["string","integer","null"]}`,
		`{"type":["null","null"]}`,
		`{"anyOf":[{"type":"string"},{"type":"integer"}]}`,
		`{"anyOf":[{"type":"string"},{"type":"integer"},{"type":"null"}]}`,
		`{"anyOf":[{"anyOf":[{"type":"string"},{"type":"null"}]},{"type":"null"}]}`,
		`{"anyOf":[{"type":["string","null"]},{"type":"null"}]}`,
		`{"anyOf":[{"type":"string"},{"type":"null","description":""}]}`,
		`{"anyOf":[{"type":"string"},{"type":"null"}],"type":"string"}`,
		`{"anyOf":[{"type":"integer"},{"type":"null"}],"maximum":null}`,
		`{"anyOf":[{"type":"null"},{"type":"null"}]}`,
		`{"anyOf":[{},{"type":"null"}]}`,
		`{"anyOf":[{"type":"string"},{"type":"null"}],"description":""}`,
	} {
		t.Run(property, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			var calls atomic.Int32
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeReviewToolReply(t, w, "inspect_component", `{"value":null}`)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			job := chatReviewJob("unsupported-union", `{"type":"object","properties":{"value":`+property+`},"required":["value"],"additionalProperties":false}`)
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			reply := readComputeReply(t, ctx, conn)
			if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "CHAT_INVALID_REQUEST") || calls.Load() != 0 {
				t.Fatalf("unsupported union/combinator must be refused before inference: type=%s error=%q calls=%d", reply.Type, reply.Error, calls.Load())
			}
		})
	}
}

func TestPairedChatNullableConstraintsAndNestedDuplicates(t *testing.T) {
	for _, example := range []struct {
		name, property, args string
		ok                   bool
	}{
		{"anyof-literal-null", `{"anyOf":[{"type":"string"},{"type":"null"}]}`, `{"value":null}`, true},
		{"anyof-enum-null-alternative", `{"anyOf":[{"type":"string","enum":["main"]},{"type":"null"}]}`, `{"value":null}`, true},
		{"anyof-enum-nonnull-restriction", `{"anyOf":[{"type":"null"},{"type":"string","enum":["main"]}]}`, `{"value":"other"}`, false},
		{"anyof-integer-upper-bound", `{"anyOf":[{"type":"integer","minimum":1,"maximum":20},{"type":"null"}]}`, `{"value":21}`, false},
		{"anyof-integer-valid-boundary", `{"anyOf":[{"type":"integer","minimum":1,"maximum":20},{"type":"null"}]}`, `{"value":20}`, true},
		{"anyof-string-lower-bound", `{"anyOf":[{"type":"string","minLength":1},{"type":"null"}]}`, `{"value":""}`, false},
		{"required-null-present", `{"type":["null","integer"],"minimum":1,"maximum":20}`, `{"value":null}`, true},
		{"required-null-missing", `{"type":["integer","null"]}`, `{}`, false},
		{"enum-still-restricts-null", `{"type":["string","null"],"enum":["main"]}`, `{"value":null}`, false},
		{"enum-explicit-null", `{"type":["string","null"],"enum":["main",null]}`, `{"value":null}`, true},
		{"const-explicit-null", `{"type":["string","null"],"const":null}`, `{"value":null}`, true},
		{"nested-null", `{"type":["object","null"],"properties":{"sheet":{"type":["string","null"]}},"required":["sheet"],"additionalProperties":false}`, `{"value":{"sheet":null}}`, true},
		{"nested-duplicate", `{"type":["object","null"],"properties":{"sheet":{"type":["string","null"]}},"required":["sheet"],"additionalProperties":false}`, `{"value":{"sheet":null,"sheet":"other"}}`, false},
	} {
		t.Run(example.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeReviewToolReply(t, w, "inspect_component", example.args)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			job := chatReviewJob("nullable-controls", `{"type":"object","properties":{"value":`+example.property+`},"required":["value"],"additionalProperties":false}`)
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			reply := readChatResult(t, ctx, conn, "nullable-controls")
			if reply.OK != example.ok || (!example.ok && !strings.HasPrefix(reply.Error, "CHAT_INVALID_TOOL_CALL")) {
				t.Fatalf("nullable argument conformance: ok=%v want=%v error=%q", reply.OK, example.ok, reply.Error)
			}
		})
	}
}

func TestPairedChatStringLengthUsesUnicodeCodePoints(t *testing.T) {
	for _, example := range []struct {
		name, args string
		ok         bool
	}{
		{"emoji-one-code-point", `{"value":"😀"}`, true},
		{"escaped-surrogate-pair-one-code-point", `{"value":"\ud83d\ude00"}`, true},
		{"combining-sequence-two-code-points", `{"value":"e\u0301"}`, false},
	} {
		t.Run(example.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeReviewToolReply(t, w, "inspect_component", example.args)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			job := chatReviewJob("unicode-length", `{"type":"object","properties":{"value":{"type":"string","minLength":1,"maxLength":1}},"required":["value"],"additionalProperties":false}`)
			if err := wsjson.Write(ctx, conn, job); err != nil {
				t.Fatal(err)
			}
			reply := readChatResult(t, ctx, conn, "unicode-length")
			if reply.OK != example.ok || (!example.ok && !strings.HasPrefix(reply.Error, "CHAT_INVALID_TOOL_CALL")) {
				t.Fatalf("Unicode code-point length conformance: ok=%v want=%v error=%q", reply.OK, example.ok, reply.Error)
			}
		})
	}
}

func TestPairedChatInitialProbeUsesLatestConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	started, release := make(chan struct{}), make(chan struct{})
	escape := make(chan struct{})
	var probes atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"fixture-vision:4b"},{"id":"fixture-chat:7b"}]}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			if !readReviewProbeRequest(t, w, r) {
				return
			}
			if probes.Add(1) == 1 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				case <-escape:
					return
				}
			}
			writeReviewToolReply(t, w, "dtx_probe", `{"ok":true}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() { close(escape); model.CloseClientConnections(); model.Close() })
	conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("initial probe did not start")
	}
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "config", "visionModels": []string{"fixture-not-installed"}}); err != nil {
		t.Fatal(err)
	}
	// A real early job refusal acknowledges that the preceding config was read.
	writeChatFixtureJob(t, ctx, conn, "before-readiness")
	if reply := readChatResult(t, ctx, conn, "before-readiness"); reply.OK || !strings.HasPrefix(reply.Error, "CHAT_MODEL_UNAVAILABLE") {
		t.Fatal("job before verified readiness must be refused")
	}
	close(release)
	capabilities := readComputeReply(t, ctx, conn)
	if capabilities.Type != "capabilities" || slices.Contains(capabilities.Capabilities, "llm.vision") || !slices.Contains(capabilities.Capabilities, "llm.chat") {
		t.Fatalf("initial probe published stale vision readiness: %v", capabilities.Capabilities)
	}
}

func TestPairedChatQueuedCapabilityUsesLatestConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("capability probing must not run a user job")
	})
	previousBase := llmBase
	llmBase = model.URL + "/v1"
	t.Cleanup(func() { llmBase = previousBase })

	connections := make(chan *websocket.Conn, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/compute/ws" || r.Header.Get("Authorization") != "Bearer synthetic-queued-token" {
			t.Error("expected the authenticated paired compute route")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connections <- conn
	}))
	t.Cleanup(backend.Close)
	client, _, err := websocket.Dial(ctx, wsURL(backend.URL), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer synthetic-queued-token"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.CloseNow() })
	var backendConn *websocket.Conn
	select {
	case backendConn = <-connections:
	case <-ctx.Done():
		t.Fatal("paired connection did not start")
	}
	t.Cleanup(func() { backendConn.CloseNow() })

	// Own the real connected handler only to schedule its write mutex; all
	// configs and observations still cross the authenticated websocket.
	a := &agent{ctx: ctx, conn: client,
		settings: LLMSettings{BackendURL: llmBase, ChatModel: "fixture-chat:7b", ChatContextTokens: 8192},
		jobs:     map[string]context.CancelFunc{}, probeRequested: make(chan struct{}, 1)}
	probeFinished := make(chan struct{}, 1)
	statusMu.Lock()
	previousWatchers := statusWatchers
	statusWatchers = append(append([]func(string){}, previousWatchers...), func(s string) {
		if s == statusConnected {
			select {
			case probeFinished <- struct{}{}:
			default:
			}
		}
	})
	statusMu.Unlock()
	t.Cleanup(func() {
		statusMu.Lock()
		statusWatchers = previousWatchers
		statusMu.Unlock()
	})

	configured := make(chan struct{}, 2)
	readerDone, probeDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			_, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			a.dispatch(data)
			configured <- struct{}{}
		}
	}()
	go func() { defer close(probeDone); a.reprobeLoop() }()
	t.Cleanup(func() {
		cancel()
		client.CloseNow()
		for _, done := range []chan struct{}{readerDone, probeDone} {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("connected handler did not stop")
			}
		}
	})

	a.writeMu.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			a.writeMu.Unlock()
		}
	})
	writeConfig := func(models []string) {
		t.Helper()
		if err := wsjson.Write(ctx, backendConn, map[string]any{"type": "config", "visionModels": models}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-configured:
		case <-ctx.Done():
			t.Fatal("config reader blocked behind the capability writer")
		}
	}
	writeConfig([]string{"fixture-vision:4b"})
	select {
	case <-probeFinished:
	case <-ctx.Done():
		t.Fatal("initial model/tool probe did not finish")
	}
	// The first successful probe passed its state check and must now wait for
	// writeMu. Change generation through the reader before letting it publish.
	writeConfig([]string{"fixture-not-installed"})
	a.writeMu.Unlock()
	locked = false
	capabilities := readComputeReply(t, ctx, backendConn)
	if capabilities.Type != "capabilities" || slices.Contains(capabilities.Capabilities, "llm.vision") || !slices.Contains(capabilities.Capabilities, "llm.chat") {
		t.Fatalf("queued publication used an obsolete config: %v", capabilities.Capabilities)
	}
}

func TestPairedChatRejectsNullSchemaOperandsBeforeInference(t *testing.T) {
	for _, keyword := range []string{
		"type", "description", "title", "properties", "required", "additionalProperties", "items", "enum",
		"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems", "anyOf",
	} {
		t.Run(keyword, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			var calls atomic.Int32
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				writeReviewToolReply(t, w, "inspect_component", `{"value":"unrestricted"}`)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			property := map[string]any{"type": "string", keyword: nil}
			if keyword == "minimum" || keyword == "maximum" {
				property["type"] = "number"
			} else if keyword == "minItems" || keyword == "maxItems" {
				property["type"] = "array"
				property["items"] = map[string]any{"type": "string"}
			}
			schema, err := json.Marshal(map[string]any{
				"type": "object", "properties": map[string]any{"value": property},
				"required": []string{"value"}, "additionalProperties": false,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := wsjson.Write(ctx, conn, chatReviewJob("null-operand-"+keyword, string(schema))); err != nil {
				t.Fatal(err)
			}
			reply := readComputeReply(t, ctx, conn)
			if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "CHAT_INVALID_REQUEST") || calls.Load() != 0 {
				t.Fatalf("present null %s must fail before inference: type=%s error=%q calls=%d", keyword, reply.Type, reply.Error, calls.Load())
			}
		})
	}
}

func TestPairedChatRejectsUnpairedSurrogatesBeforeInference(t *testing.T) {
	for _, example := range []struct{ name, schema string }{
		{"enum-high", `{"type":"object","properties":{"value":{"type":"string","enum":["\ud800"]}},"required":["value"],"additionalProperties":false}`},
		{"enum-low", `{"type":"object","properties":{"value":{"type":"string","enum":["\udfff"]}},"required":["value"],"additionalProperties":false}`},
		{"const-high", `{"type":"object","properties":{"value":{"type":"string","const":"\ud801"}},"required":["value"],"additionalProperties":false}`},
		{"property-high", `{"type":"object","properties":{"\ud800":{"type":"string"}},"required":["\ud800"],"additionalProperties":false}`},
		{"property-low", `{"type":"object","properties":{"\udfff":{"type":"string"}},"required":["\udfff"],"additionalProperties":false}`},
		{"nested-property-high", `{"type":"object","properties":{"nested":{"type":"object","properties":{"\ud801":{"type":"string"}},"required":["\ud801"],"additionalProperties":false}},"required":["nested"],"additionalProperties":false}`},
	} {
		t.Run(example.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			var calls atomic.Int32
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				io.WriteString(w, fixtureChatReply)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			if err := wsjson.Write(ctx, conn, chatReviewJob("unpaired-schema", example.schema)); err != nil {
				t.Fatal(err)
			}
			reply := readComputeReply(t, ctx, conn)
			if reply.Type != "job.result" || reply.OK || !strings.HasPrefix(reply.Error, "CHAT_INVALID_REQUEST") || calls.Load() != 0 {
				t.Fatalf("unpaired schema escape must fail before inference: type=%s error=%q calls=%d", reply.Type, reply.Error, calls.Load())
			}
		})
	}
}

func TestPairedChatUnicodeEscapesInToolArguments(t *testing.T) {
	for _, example := range []struct {
		name, properties, required, args string
		ok                               bool
	}{
		{"nested-high", `{"nested":{"type":"object","properties":{"value":{"type":"string","enum":["�"]}},"required":["value"],"additionalProperties":false}}`, `["nested"]`, `{"nested":{"value":"\ud800"}}`, false},
		{"nested-low", `{"nested":{"type":"object","properties":{"value":{"type":"string","enum":["�"]}},"required":["value"],"additionalProperties":false}}`, `["nested"]`, `{"nested":{"value":"\udfff"}}`, false},
		{"argument-key-high", `{"�":{"type":"string"}}`, `["�"]`, `{"\ud800":"synthetic"}`, false},
		{"argument-key-low", `{"�":{"type":"string"}}`, `["�"]`, `{"\udfff":"synthetic"}`, false},
		{"high-followed-by-nonsurrogate", `{"value":{"type":"string"}}`, `["value"]`, `{"value":"\ud800\u0041"}`, false},
		{"valid-astral-key-and-value", `{"\ud83d\ude00":{"type":"string","enum":["\ud83d\ude00"],"minLength":1,"maxLength":1}}`, `["\ud83d\ude00"]`, `{"\ud83d\ude00":"\ud83d\ude00"}`, true},
		{"literal-backslash-high", `{"value":{"type":"string","enum":["\\ud800"]}}`, `["value"]`, `{"value":"\\ud800"}`, true},
		{"literal-backslash-low", `{"value":{"type":"string","enum":["\\udfff"]}}`, `["value"]`, `{"value":"\\udfff"}`, true},
		{"distinct-literal-backslashes", `{"value":{"type":"string","enum":["\\ud800"]}}`, `["value"]`, `{"value":"\\ud801"}`, false},
		{"literal-is-not-replacement-character", `{"value":{"type":"string","enum":["\\ud800"]}}`, `["value"]`, `{"value":"�"}`, false},
		{"literal-after-escaped-quote", `{"value":{"type":"string","enum":["quoted\"\\ud800"]}}`, `["value"]`, `{"value":"quoted\"\\ud800"}`, true},
	} {
		t.Run(example.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			model := chatModelServer(t, func(w http.ResponseWriter, r *http.Request) {
				writeReviewToolReply(t, w, "inspect_component", example.args)
			})
			conn := connectPairedChat(t, ctx, model.URL+"/v1", "fixture-chat:7b", 8192).Conn
			readComputeReply(t, ctx, conn)
			schema := `{"type":"object","properties":` + example.properties + `,"required":` + example.required + `,"additionalProperties":false}`
			if err := wsjson.Write(ctx, conn, chatReviewJob("unicode-arguments", schema)); err != nil {
				t.Fatal(err)
			}
			reply := readChatResult(t, ctx, conn, "unicode-arguments")
			if reply.OK != example.ok || (!example.ok && !strings.HasPrefix(reply.Error, "CHAT_INVALID_TOOL_CALL")) {
				t.Fatalf("tool argument escape conformance: ok=%v want=%v error=%q", reply.OK, example.ok, reply.Error)
			}
			if example.ok {
				var completion struct {
					Choices []struct {
						Message struct {
							ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
						}
					}
				}
				if json.Unmarshal(reply.Result, &completion) != nil || len(completion.Choices) != 1 || len(completion.Choices[0].Message.ToolCalls) != 1 || completion.Choices[0].Message.ToolCalls[0].Function.Arguments != example.args {
					t.Fatal("valid tool arguments must preserve the original JSON string")
				}
			}
		})
	}
}
