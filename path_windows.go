//go:build windows

package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// addToPath appends the binary's directory to the per-user PATH (HKCU\Environment),
// which needs no admin. New processes (freshly opened terminals) pick it up.
// ponytail: no WM_SETTINGCHANGE broadcast — already-open shells just need a restart.
func addToPath() error {
	dir, err := selfDir()
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	cur, _, _ := k.GetStringValue("Path")
	if pathContains(cur, dir) {
		return nil // already present
	}
	next := dir
	if cur != "" {
		next = strings.TrimRight(cur, ";") + ";" + dir
	}
	return k.SetExpandStringValue("Path", next)
}

// removeFromPath drops the binary's directory from the per-user PATH.
func removeFromPath() error {
	dir, err := selfDir()
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, `Environment`, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	cur, _, err := k.GetStringValue("Path")
	if err != nil {
		return nil // no Path value → nothing to remove
	}
	var kept []string
	for _, p := range strings.Split(cur, ";") {
		if p != "" && !samePath(p, dir) {
			kept = append(kept, p)
		}
	}
	return k.SetExpandStringValue("Path", strings.Join(kept, ";"))
}

func selfDir() (string, error) {
	exe, err := selfPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func samePath(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, `\`), strings.TrimRight(b, `\`))
}

func pathContains(pathVal, dir string) bool {
	for _, p := range strings.Split(pathVal, ";") {
		if samePath(p, dir) {
			return true
		}
	}
	return false
}
