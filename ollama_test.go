package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHasVisionModel(t *testing.T) {
	installed := []string{"gemma3:4b", "llama3.2:latest"}

	cases := []struct {
		name       string
		acceptable []string
		want       bool
	}{
		{"exact tag match", []string{"gemma3:4b"}, true},
		{"base name matches installed tag", []string{"gemma3"}, true},
		{"acceptable tag vs installed base", []string{"llama3.2"}, true},
		{"not installed", []string{"qwen2.5vl"}, false},
		{"empty acceptable", []string{}, false},
	}
	for _, c := range cases {
		if got := hasVisionModel(installed, c.acceptable); got != c.want {
			t.Errorf("%s: hasVisionModel(%v) = %v, want %v", c.name, c.acceptable, got, c.want)
		}
	}
}

// modelsFixture mimics the OpenAI-compatible GET /v1/models response.
const modelsFixture = `{"data":[
  {"id":"gemma3:4b"},
  {"id":"llama3.2:latest"}
]}`

func TestProbeModelsParsesModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(modelsFixture))
	}))
	defer srv.Close()

	old := llmBase
	llmBase = srv.URL + "/v1"
	defer func() { llmBase = old }()

	names, err := probeModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "gemma3:4b" {
		t.Fatalf("probeModels returned %v", names)
	}
	if !hasVisionModel(names, []string{"gemma3"}) {
		t.Errorf("expected gemma3 to be recognized from probe result")
	}
}

func TestProbeModelsUnreachable(t *testing.T) {
	old := llmBase
	llmBase = "http://127.0.0.1:0/v1" // invalid port → guaranteed failure
	defer func() { llmBase = old }()
	if _, err := probeModels(context.Background()); err == nil {
		t.Error("expected error probing unreachable server")
	}
}

func TestChatSendsOpenAIRequest(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	old := llmBase
	llmBase = srv.URL + "/v1"
	defer func() { llmBase = old }()

	schema := json.RawMessage(`{"type":"object"}`)
	out, err := chat(context.Background(), "gemma3", "describe", []string{"data:image/png;base64,AAAA"}, schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"ok":true}` {
		t.Fatalf("chat returned %s", out)
	}

	// Request must be OpenAI-shaped: content parts (text + image_url) + response_format.
	content := gotBody["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want text + image content parts, got %v", content)
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("first part should be text, got %v", content[0])
	}
	img := content[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("second part should be image_url, got %v", img)
	}
	if url := img["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,AAAA" {
		t.Errorf("image url = %v", url)
	}
	if rf, ok := gotBody["response_format"].(map[string]any); !ok || rf["type"] != "json_schema" {
		t.Errorf("response_format = %v, want json_schema", gotBody["response_format"])
	}
}

func TestParseLLMBase(t *testing.T) {
	// Ollama commented, LM Studio active → LM Studio wins.
	if got := parseLLMBase([]byte("# note\n# http://localhost:11434/v1\nhttp://localhost:1234/v1\n")); got != "http://localhost:1234/v1" {
		t.Errorf("parseLLMBase = %q, want LM Studio url", got)
	}
	// First uncommented line wins; surrounding whitespace trimmed.
	if got := parseLLMBase([]byte("\n#comment\n  http://a/v1  \nhttp://b/v1\n")); got != "http://a/v1" {
		t.Errorf("parseLLMBase = %q, want http://a/v1", got)
	}
	// All commented → empty (caller falls back to default).
	if got := parseLLMBase([]byte("# only\n# comments\n")); got != "" {
		t.Errorf("parseLLMBase = %q, want empty", got)
	}
}

func TestLoadLLMBaseCreatesAndReads(t *testing.T) {
	// Redirect the config dir so we don't touch the real one (both vars cover
	// darwin, which uses HOME, and linux, which uses XDG_CONFIG_HOME).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// First run: file missing → writes default, returns the Ollama default.
	if got := loadLLMBase(); got != "http://localhost:11434/v1" {
		t.Fatalf("first loadLLMBase = %q, want Ollama default", got)
	}
	path, err := llmConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("llm.txt was not created: %v", err)
	}

	// User switches to LM Studio → next load returns it.
	if err := os.WriteFile(path, []byte("# http://localhost:11434/v1\nhttp://localhost:1234/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadLLMBase(); got != "http://localhost:1234/v1" {
		t.Fatalf("after edit loadLLMBase = %q, want LM Studio url", got)
	}
}

func TestLoadLLMBasePrefersFileNextToBinary(t *testing.T) {
	// The release ships llm.txt next to the binary; that copy must win over the
	// per-user config dir. Drop one beside the test executable and check it wins.
	exe, err := os.Executable()
	if err != nil {
		t.Skip("cannot locate test executable")
	}
	p := filepath.Join(filepath.Dir(exe), "llm.txt")
	if _, err := os.Stat(p); err == nil {
		t.Skip("llm.txt already exists next to test binary")
	}
	if err := os.WriteFile(p, []byte("# http://localhost:11434/v1\nhttp://localhost:1234/v1\n"), 0o644); err != nil {
		t.Skipf("cannot write next to test binary: %v", err)
	}
	defer os.Remove(p)

	// Point the config dir elsewhere so only the sibling file can satisfy this.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if got := loadLLMBase(); got != "http://localhost:1234/v1" {
		t.Fatalf("loadLLMBase = %q, want the sibling llm.txt url", got)
	}
}

func TestPickModel(t *testing.T) {
	acceptable := []string{"gemma3:4b", "gemma3:12b", "llava:13b", "llava"}
	cases := []struct {
		name      string
		preferred string
		installed []string
		want      string
	}{
		{"exact preferred installed", "gemma3:4b", []string{"llava", "gemma3:4b"}, "gemma3:4b"},
		{"base-name substitute", "gemma3:4b", []string{"gemma3:12b"}, "gemma3:12b"},
		{"acceptable fallback", "gemma3:4b", []string{"mistral:7b", "llava:13b"}, "llava:13b"},
		{"nothing usable keeps preference", "gemma3:4b", []string{"mistral:7b"}, "gemma3:4b"},
		{"no models installed", "gemma3:4b", nil, "gemma3:4b"},
	}
	for _, c := range cases {
		if got := pickModel(c.preferred, c.installed, acceptable); got != c.want {
			t.Errorf("%s: pickModel = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestToOllamaImagePassthrough(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n rest of a png")
	if got := toOllamaImage(png); !bytes.Equal(got, png) {
		t.Error("non-webp bytes must pass through untouched")
	}
	// Valid RIFF/WEBP magic but garbage body: decode fails → original returned.
	broken := append([]byte("RIFF\x10\x00\x00\x00WEBP"), []byte("VP8 garbage")...)
	if got := toOllamaImage(broken); !bytes.Equal(got, broken) {
		t.Error("undecodable webp must fall back to original bytes")
	}
}
