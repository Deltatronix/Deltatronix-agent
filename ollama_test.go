package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// tagsFixture mimics Ollama's GET /api/tags response.
const tagsFixture = `{"models":[
  {"name":"gemma3:4b","model":"gemma3:4b"},
  {"name":"llama3.2:latest","model":"llama3.2:latest"}
]}`

func TestHasVisionModel(t *testing.T) {
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	json.Unmarshal([]byte(tagsFixture), &body)
	installed := []string{}
	for _, m := range body.Models {
		installed = append(installed, m.Name)
	}

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

func TestProbeOllamaParsesTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(tagsFixture))
	}))
	defer srv.Close()

	old := ollamaBase
	ollamaBase = srv.URL
	defer func() { ollamaBase = old }()

	names, err := probeOllama(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "gemma3:4b" {
		t.Fatalf("probeOllama returned %v", names)
	}
	if !hasVisionModel(names, []string{"gemma3"}) {
		t.Errorf("expected gemma3 to be recognized from probe result")
	}
}

func TestProbeOllamaUnreachable(t *testing.T) {
	old := ollamaBase
	ollamaBase = "http://127.0.0.1:0" // invalid port → guaranteed failure
	defer func() { ollamaBase = old }()
	if _, err := probeOllama(context.Background()); err == nil {
		t.Error("expected error probing unreachable Ollama")
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
