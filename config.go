package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the paired-agent state persisted to disk. apiUrl is the REST base
// (e.g. https://api.deltatronix.io); the WebSocket URL is derived from it.
type Config struct {
	APIURL     string `json:"apiUrl"`
	AgentID    string `json:"agentId"`
	AgentToken string `json:"agentToken"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "dtx-agent", "config.json"), nil
}

func loadConfig() (Config, error) {
	var c Config
	path, err := configPath()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

func saveConfig(c Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	// WriteFile only applies perms when creating; force 0600 on an existing file too.
	return os.Chmod(path, 0o600)
}
