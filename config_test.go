package main

import (
	"encoding/json"
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
