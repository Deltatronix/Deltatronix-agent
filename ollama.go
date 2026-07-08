package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/image/webp"
)

// llmBase is the OpenAI-compatible API base URL (ending in /v1). Ollama serves
// it at :11434/v1, LM Studio at :1234/v1, and so do llama.cpp, vLLM, LocalAI…
// It's a var (not const) so loadLLMBase and tests can point it elsewhere.
var llmBase = "http://localhost:11434/v1"

// probeModels returns the model ids the server has loaded, or an error if the
// server is unreachable. Uses the OpenAI-compatible GET /v1/models.
func probeModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/models: %s", llmBase, resp.Status)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range body.Data {
		if m.ID != "" {
			names = append(names, m.ID)
		}
	}
	return names, nil
}

// hasVisionModel reports whether any installed model satisfies the server's
// acceptable-model list. Match is exact, or on the base name (ignoring the
// `:tag`) so "gemma3:4b" installed satisfies acceptable "gemma3" and vice versa.
func hasVisionModel(installed, acceptable []string) bool {
	base := func(s string) string { return strings.SplitN(s, ":", 2)[0] }
	for _, want := range acceptable {
		for _, got := range installed {
			if got == want || base(got) == base(want) {
				return true
			}
		}
	}
	return false
}

// pickModel returns the installed model name to run: the server's preference
// when installed (exact first, then base-name match so preferred "gemma3:4b"
// resolves to an installed "gemma3:12b"), else any installed model from the
// acceptable list, else the preference verbatim (Ollama will report it missing).
// The server names its top preference blind — it only knows capabilities, not
// which models this machine actually has (ADR-0022).
func pickModel(preferred string, installed, acceptable []string) string {
	base := func(s string) string { return strings.SplitN(s, ":", 2)[0] }
	for _, got := range installed {
		if got == preferred {
			return got
		}
	}
	for _, got := range installed {
		if base(got) == base(preferred) {
			return got
		}
	}
	for _, want := range acceptable {
		for _, got := range installed {
			if got == want || base(got) == base(want) {
				return got
			}
		}
	}
	return preferred
}

// toOllamaImage makes downloaded bytes acceptable to Ollama's image loader,
// which decodes PNG/JPEG only — and the Deltatronix platform stores every
// photo as WebP (ADR-0016). WebP (sniffed by bytes, not extension) is
// transcoded to PNG; anything else passes through untouched. On a decode
// failure the original bytes pass through so Ollama surfaces the error.
func toOllamaImage(b []byte) []byte {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return b
	}
	img, err := webp.Decode(bytes.NewReader(b))
	if err != nil {
		return b
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return b
	}
	return out.Bytes()
}

// chat POSTs a vision chat request and returns the model's reply as raw JSON
// (validated). Uses the OpenAI-compatible POST /v1/chat/completions: images are
// data-URI content parts and responseSchema is sent as response_format.
func chat(ctx context.Context, model, prompt string, imageURIs []string, responseSchema json.RawMessage) (json.RawMessage, error) {
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, uri := range imageURIs {
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": uri},
		})
	}
	reqBody := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	if len(responseSchema) > 0 {
		reqBody["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "response",
				"schema": responseSchema,
				"strict": true,
			},
		}
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, llmBase+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("POST %s/chat/completions: %s: %s", llmBase, resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("chat reply had no choices")
	}
	// With response_format set, content is a JSON document — validate before returning.
	var parsed json.RawMessage
	if err := json.Unmarshal([]byte(out.Choices[0].Message.Content), &parsed); err != nil {
		return nil, fmt.Errorf("model reply was not valid JSON: %w", err)
	}
	return parsed, nil
}

// printLLMHint tells the user how to reach a local LLM server. It assumes the
// default (Ollama) but points at llm.txt so they can switch to LM Studio etc.
func printLLMHint(acceptable []string) {
	var install string
	switch runtime.GOOS {
	case "windows":
		install = "winget install Ollama.Ollama"
	case "darwin":
		install = "brew install ollama   (or download from https://ollama.com/download)"
	default:
		install = "curl -fsSL https://ollama.com/install.sh | sh"
	}
	fmt.Println("No LLM server is reachable at " + llmBase + ".")
	fmt.Println("  Install Ollama:  " + install)
	if len(acceptable) > 0 {
		fmt.Println("  Then pull a vision model:  ollama pull " + acceptable[0])
	}
	if path, err := llmConfigPath(); err == nil {
		fmt.Println("  To use LM Studio or another backend, edit " + path + " and restart.")
	}
	fmt.Println("  Waiting for a server — this agent will keep retrying.")
}

// defaultLLMTxt is the template shipped in the release (llm.txt, next to the
// binary) and written to the per-user config dir on first run. Embedding the
// same file keeps the shipped copy and the built-in default from drifting.
//
//go:embed llm.txt
var defaultLLMTxt string

func llmConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dtx-agent", "llm.txt"), nil
}

// parseLLMBase returns the first non-comment, non-blank line (trimmed), or "".
// ponytail: whole-line comments only, first url wins — no inline-comment / key=value parsing.
func parseLLMBase(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		return t
	}
	return ""
}

// loadLLMBase resolves the backend URL. It first honors an llm.txt shipped next
// to the binary (so the user can edit it before the first run), then falls back
// to a per-user copy in the config dir, writing the default there on first run.
func loadLLMBase() string {
	const def = "http://localhost:11434/v1"

	// 1. llm.txt next to the executable — the release ships one here, editable
	//    before running. Only wins if it names an active url.
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "llm.txt")
		if data, err := os.ReadFile(p); err == nil {
			if u := parseLLMBase(data); u != "" {
				log.Printf("using LLM backend config %s", p)
				return u
			}
		}
	}

	// 2. Per-user llm.txt in the config dir; create the default on first run
	//    (covers `go install` / source builds with no shipped file alongside).
	path, err := llmConfigPath()
	if err != nil {
		return def
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr == nil {
			if wErr := os.WriteFile(path, []byte(defaultLLMTxt), 0o644); wErr == nil {
				log.Printf("wrote default LLM backend config to %s", path)
			}
		}
		return def
	}
	if u := parseLLMBase(data); u != "" {
		return u
	}
	log.Printf("no active url in %s — using default %s", path, def)
	return def
}
