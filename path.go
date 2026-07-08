package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// cmdPath implements `dtx-agent path add|remove`, the headless-accessible entry
// point; the GUI button calls addToPath/removeFromPath directly. The per-OS
// implementations live in path_windows.go and path_unix.go.
func cmdPath(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "add":
		if err := addToPath(); err != nil {
			return err
		}
		fmt.Println("Added dtx-agent to PATH.")
		return nil
	case "remove":
		if err := removeFromPath(); err != nil {
			return err
		}
		fmt.Println("Removed dtx-agent from PATH.")
		return nil
	default:
		return errors.New("usage: dtx-agent path add|remove")
	}
}

// selfPath is the absolute path to the running binary, resolved through symlinks
// so autostart/PATH entries point at the real file.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}
