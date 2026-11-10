package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	// Point UserConfigDir at a temp dir: HOME drives it on both darwin
	// ($HOME/Library/Application Support) and linux ($HOME/.config, once XDG is
	// cleared). Windows uses %AppData%, so skip there.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", "")
	if base, err := os.UserConfigDir(); err != nil || !strings.HasPrefix(base, dir) {
		t.Skipf("UserConfigDir not under temp HOME here (base=%s err=%v)", base, err)
	}

	in := Config{APIURL: "https://api.deltatronix.io", AgentID: "a1", AgentToken: "secret"}
	if err := saveConfig(in); err != nil {
		t.Fatal(err)
	}

	path, _ := configPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config perms = %o, want 600", perm)
	}

	out, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round trip mismatch: %+v != %+v", out, in)
	}
}

func TestLoadConfigParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"apiUrl":"http://x","agentId":"id","agentToken":"tok"}`), 0o600)
	var c Config
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.APIURL != "http://x" || c.AgentID != "id" || c.AgentToken != "tok" {
		t.Errorf("parsed wrong: %+v", c)
	}
}

func TestIsolatedAgentProfilePaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DTX_AGENT_CONFIG_DIR", dir)
	config, _ := configPath()
	llm, _ := llmConfigPath()
	if config != filepath.Join(dir, "config.json") || llm != filepath.Join(dir, "llm.txt") || defaultLogPath() != filepath.Join(dir, "dtx-agent.log") {
		t.Fatal("profile paths differ")
	}
	if err := saveConfig(Config{AgentToken: "test-token"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(config)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("profile token is not private")
	}
	t.Setenv("DTX_AGENT_CONFIG_DIR", "relative")
	if _, err := configPath(); err == nil {
		t.Fatal("relative profile accepted")
	}
}
func TestPairingCodeStdinIsBounded(t *testing.T) {
	code, err := readPairingCode(strings.NewReader("  ABC123\n"))
	if err != nil || code != "ABC123" {
		t.Fatal("valid code rejected")
	}
	for _, bad := range []string{"", strings.Repeat("x", 129), "ABC\nDEF", "ABC\x00DEF"} {
		if _, err := readPairingCode(strings.NewReader(bad)); err == nil {
			t.Fatal("invalid code accepted")
		}
	}
}

func TestDefaultAgentProfilePathsRemainCompatible(t *testing.T) {
	t.Setenv("DTX_AGENT_CONFIG_DIR", "")
	base, err := os.UserConfigDir()
	if err != nil {
		t.Skip(err)
	}
	config, _ := configPath()
	llm, _ := llmConfigPath()
	if config != filepath.Join(base, "dtx-agent", "config.json") || llm != filepath.Join(base, "dtx-agent", "llm.txt") || defaultLogPath() != filepath.Join(base, "dtx-agent", "dtx-agent.log") {
		t.Fatal("default paths changed")
	}
}
func TestPairStdinDoesNotPrintPairingSecrets(t *testing.T) {
	t.Setenv("DTX_AGENT_CONFIG_DIR", t.TempDir())
	const code = "TESTPAIRSECRET"
	const token = "TESTTOKENKEEPPRIVATE"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["code"] != code {
			t.Error("stdin pairing code was not sent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agentId":"qa-agent","agentToken":"TESTTOKENKEEPPRIVATE"}`))
	}))
	defer server.Close()
	inR, inW, _ := os.Pipe()
	outR, outW, _ := os.Pipe()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin = inR
	os.Stdout = outW
	defer func() { os.Stdin = oldIn; os.Stdout = oldOut; inR.Close(); outR.Close() }()
	io.WriteString(inW, code+"\n")
	inW.Close()
	err := cmdPair([]string{"--code-stdin", "--api", server.URL})
	outW.Close()
	output, _ := io.ReadAll(outR)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), code) || strings.Contains(string(output), token) {
		t.Fatal("pairing secret printed")
	}
	cfg, err := loadConfig()
	if err != nil || cfg.AgentToken != token {
		t.Fatal("isolated pairing did not persist")
	}
}
