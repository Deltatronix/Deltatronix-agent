package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxChatBytes = 64 << 10
	chatTimeout  = 180 * time.Second
)

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Strict      bool            `json:"strict"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type chatPayload struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Tools     []chatTool    `json:"tools"`
	MaxTokens int           `json:"maxTokens"`
}

type chatAssignment struct {
	Type      string      `json:"type"`
	JobType   string      `json:"jobType"`
	JobID     string      `json:"jobId"`
	ExpiresAt time.Time   `json:"expiresAt"`
	Payload   chatPayload `json:"payload"`
}

type chatCompletion struct {
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		FinishReason string      `json:"finish_reason"`
		Message      chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Total      int `json:"total_tokens"`
	} `json:"usage"`
}

// Chat never follows a local server redirect or an environment-configured proxy.
var localChatHTTP = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("CHAT_BACKEND_UNAVAILABLE")
		}
		if host == "localhost" {
			host = "127.0.0.1"
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, errors.New("CHAT_BACKEND_UNAVAILABLE: loopback required")
		}
		dialer := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}()

func localChatSettings(s LLMSettings) bool {
	u, err := url.Parse(s.BackendURL)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || strings.TrimRight(u.Path, "/") != "/v1" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return (host == "localhost" || (ip != nil && ip.IsLoopback())) &&
		validChatName(s.ChatModel, 200) && s.ChatContextTokens > 0 && s.ChatContextTokens <= 1<<20
}

func validChatName(s string, limit int) bool {
	return s != "" && len(s) <= limit && !strings.ContainsAny(s, "\r\n\x00")
}

func readChatBytes(r io.Reader, limit int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit+1)))
	if err != nil || len(b) > limit {
		return nil, errors.New("CHAT_RESPONSE_LIMIT: local response exceeded its limit or could not be read")
	}
	return b, nil
}

func decodeChatAssignment(data []byte, s LLMSettings) (chatAssignment, error) {
	var job chatAssignment
	if len(data) > maxChatBytes || decodeChatJSON(data, &job) != nil || job.Type != "job.assign" ||
		job.JobType != "LLM_CHAT_TOOLS" || !validChatName(job.JobID, 128) {
		return job, errors.New("CHAT_INVALID_REQUEST: use job.assign with jobType LLM_CHAT_TOOLS and a bounded payload")
	}
	if !localChatSettings(s) || job.Payload.Model != s.ChatModel {
		return job, errors.New("CHAT_MODEL_UNAVAILABLE: explicitly select an installed model and positive context cap in llm.txt")
	}
	if time.Until(job.ExpiresAt) <= 0 || time.Until(job.ExpiresAt) > 5*time.Minute {
		return job, errors.New("CHAT_EXPIRED: expiresAt must be in the next five minutes")
	}
	if err := validateChatPayload(job.Payload, s.ChatContextTokens); err != nil {
		return job, err
	}
	return job, nil
}

func decodeChatJSON(data []byte, v any) error {
	if err := checkChatJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

// encoding/json otherwise accepts duplicate members and replaces invalid text.
func checkChatJSON(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON depth limit")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON member")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("invalid object")
			}
		case json.Delim('['):
			for d.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("invalid array")
			}
		case json.Delim('}'), json.Delim(']'):
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func validateChatPayload(p chatPayload, contextTokens int) error {
	bad := errors.New("CHAT_INVALID_REQUEST: use bounded text messages, strict function tools and a positive maxTokens")
	if len(p.Messages) == 0 || len(p.Messages) > 64 || len(p.Tools) > 20 || p.MaxTokens < 1 || p.MaxTokens > 8192 {
		return bad
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > maxChatBytes || len(data)+p.MaxTokens+1024 > contextTokens {
		return errors.New("CHAT_CONTEXT_LIMIT: narrow messages/tools or increase the explicit context cap")
	}
	names := map[string]bool{}
	for _, tool := range p.Tools {
		if tool.Type != "function" || !tool.Function.Strict || !validToolName(tool.Function.Name) || names[tool.Function.Name] || len(tool.Function.Description) > 2048 {
			return bad
		}
		// Parameters pass through untouched; the browser runtime owns argument
		// validation and returns a recoverable refusal to the model.
		var params map[string]json.RawMessage
		if json.Unmarshal(tool.Function.Parameters, &params) != nil || params == nil {
			return bad
		}
		names[tool.Function.Name] = true
	}
	pending, seenCalls := map[string]bool{}, map[string]bool{}
	for _, m := range p.Messages {
		if !slices.Contains([]string{"system", "user", "assistant", "tool"}, m.Role) || len(m.Content) > 16<<10 || len(m.ToolCalls) > 8 {
			return bad
		}
		var text string
		if string(m.Content) != "null" && json.Unmarshal(m.Content, &text) != nil {
			return bad
		}
		if string(m.Content) == "null" && (m.Role != "assistant" || len(m.ToolCalls) == 0) {
			return bad
		}
		if m.Role == "tool" {
			if !pending[m.ToolCallID] || len(m.ToolCalls) != 0 {
				return bad
			}
			delete(pending, m.ToolCallID)
			continue
		}
		if len(pending) != 0 || m.ToolCallID != "" || (m.Role != "assistant" && len(m.ToolCalls) != 0) {
			return bad
		}
		for _, call := range m.ToolCalls {
			if seenCalls[call.ID] || !validChatCall(call, p.Tools) {
				return bad
			}
			seenCalls[call.ID], pending[call.ID] = true, true
		}
	}
	if len(pending) != 0 {
		return bad
	}
	return nil
}

func callLocalChat(ctx context.Context, s LLMSettings, p chatPayload) (json.RawMessage, error) {
	data, err := json.Marshal(map[string]any{"model": p.Model, "messages": p.Messages, "tools": p.Tools, "stream": false, "max_tokens": p.MaxTokens})
	if err != nil || len(data) > maxChatBytes {
		return nil, errors.New("CHAT_INVALID_REQUEST")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BackendURL, "/")+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("CHAT_BACKEND_UNAVAILABLE")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := localChatHTTP.Do(req)
	if err != nil {
		return nil, errors.New("CHAT_BACKEND_UNAVAILABLE: check the local server or cancellation/expiry")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CHAT_BACKEND_STATUS: local server returned %d", resp.StatusCode)
	}
	body, err := readChatBytes(resp.Body, maxChatBytes)
	if err != nil {
		return nil, err
	}
	var result chatCompletion
	if checkChatJSON(body) != nil || json.Unmarshal(body, &result) != nil || result.Model != p.Model || len(result.Choices) != 1 || result.Choices[0].Index != 0 {
		return nil, errors.New("CHAT_INVALID_RESPONSE: expected one completion from the selected model")
	}
	choice := result.Choices[0]
	if choice.FinishReason == "length" {
		return nil, errors.New("CHAT_TRUNCATED: the local model hit maxTokens before finishing")
	}
	if choice.Message.Role != "assistant" || !slices.Contains([]string{"stop", "tool_calls"}, choice.FinishReason) || len(choice.Message.ToolCalls) > 8 || len(choice.Message.Content) > 16<<10 {
		return nil, errors.New("CHAT_INVALID_RESPONSE")
	}
	var text string
	if choice.Message.ToolCallID != "" || (string(choice.Message.Content) != "null" && json.Unmarshal(choice.Message.Content, &text) != nil) ||
		(len(choice.Message.ToolCalls) == 0 && string(choice.Message.Content) == "null") ||
		((choice.FinishReason == "tool_calls") != (len(choice.Message.ToolCalls) > 0)) || result.Usage.Prompt < 0 || result.Usage.Completion < 0 || result.Usage.Completion > p.MaxTokens || result.Usage.Total != result.Usage.Prompt+result.Usage.Completion {
		return nil, errors.New("CHAT_INVALID_RESPONSE: invalid assistant completion or usage")
	}
	seen := map[string]bool{}
	for _, call := range choice.Message.ToolCalls {
		if seen[call.ID] || !validChatCall(call, p.Tools) {
			return nil, errors.New("CHAT_INVALID_TOOL_CALL: expected an advertised function with JSON object arguments")
		}
		seen[call.ID] = true
	}
	normalized, err := json.Marshal(result)
	if err != nil || len(normalized) > 60<<10 {
		return nil, errors.New("CHAT_RESPONSE_LIMIT: normalized completion exceeded 60 KiB")
	}
	return normalized, nil
}

func probeLocalChat(ctx context.Context, s LLMSettings, installed []string) bool {
	if !localChatSettings(s) || !slices.Contains(installed, s.ChatModel) {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, chatTimeout)
	defer cancel()
	var p chatPayload
	_ = json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"Call dtx_probe once with ok true. Do not answer with text."}],"tools":[{"type":"function","function":{"name":"dtx_probe","description":"Confirm tool support.","strict":true,"parameters":{"type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}}}],"maxTokens":128}`), &p)
	p.Model = s.ChatModel
	if validateChatPayload(p, s.ChatContextTokens) != nil {
		return false
	}
	data, err := callLocalChat(probeCtx, s, p)
	var result chatCompletion
	if err != nil || json.Unmarshal(data, &result) != nil || len(result.Choices[0].Message.ToolCalls) != 1 {
		return false
	}
	call := result.Choices[0].Message.ToolCalls[0]
	var args struct {
		OK bool `json:"ok"`
	}
	return call.Function.Name == "dtx_probe" && decodeChatJSON([]byte(call.Function.Arguments), &args) == nil && args.OK
}

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// A call must name an advertised tool and carry a JSON object of at most 8 KiB.
// Argument types are the browser runtime's to check.
func validChatCall(call chatToolCall, tools []chatTool) bool {
	if call.Type != "function" || !validChatName(call.ID, 128) || len(call.Function.Arguments) > 8192 {
		return false
	}
	var args map[string]json.RawMessage
	if decodeChatJSON([]byte(call.Function.Arguments), &args) != nil || args == nil {
		return false
	}
	return slices.ContainsFunc(tools, func(tool chatTool) bool { return tool.Function.Name == call.Function.Name })
}
