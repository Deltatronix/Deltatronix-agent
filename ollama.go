package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
)

// ollamaBase is a var (not const) so tests can point it at a stub server.
var ollamaBase = "http://localhost:11434"

// probeOllama returns the model names Ollama has locally, or an error if Ollama
// is unreachable.
func probeOllama(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ollamaBase+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama /api/tags: %s", resp.Status)
	}
	var body struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var names []string
	for _, m := range body.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		} else if m.Model != "" {
			names = append(names, m.Model)
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

// ollamaChat POSTs a vision chat request and returns the model's reply as raw
// JSON (validated). responseSchema is passed to Ollama as `format`.
func ollamaChat(ctx context.Context, model, prompt string, imagesB64 []string, responseSchema json.RawMessage) (json.RawMessage, error) {
	reqBody := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": prompt, "images": imagesB64}},
	}
	if len(responseSchema) > 0 {
		reqBody["format"] = responseSchema
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaBase+"/api/chat", bytes.NewReader(data))
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
		return nil, fmt.Errorf("ollama /api/chat: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	// With `format` set, content is a JSON document — validate before returning.
	var parsed json.RawMessage
	if err := json.Unmarshal([]byte(out.Message.Content), &parsed); err != nil {
		return nil, fmt.Errorf("model reply was not valid JSON: %w", err)
	}
	return parsed, nil
}

// printInstallHint tells the user how to install Ollama and pull a usable model.
func printInstallHint(acceptable []string) {
	var install string
	switch runtime.GOOS {
	case "windows":
		install = "winget install Ollama.Ollama"
	case "darwin":
		install = "brew install ollama   (or download from https://ollama.com/download)"
	default:
		install = "curl -fsSL https://ollama.com/install.sh | sh"
	}
	fmt.Println("Ollama is not reachable at " + ollamaBase + ".")
	fmt.Println("  Install it:  " + install)
	if len(acceptable) > 0 {
		fmt.Println("  Then pull a vision model:  ollama pull " + acceptable[0])
	}
	fmt.Println("  Waiting for Ollama — this agent will keep retrying.")
}
