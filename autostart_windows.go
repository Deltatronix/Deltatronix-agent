//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "dtx-agent"

// enableAutostart sets an HKCU Run value (no admin). mode is ignored on Windows.
func enableAutostart(headless bool, _ string) error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()

	cmd := `"` + exe + `"`
	for _, a := range autostartArgs(headless) {
		cmd += " " + a
	}
	return k.SetStringValue(runValueName, cmd)
}

func disableAutostart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(runValueName); err != nil && !strings.Contains(err.Error(), "cannot find") {
		return err
	}
	return nil
}

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValueName)
	return err == nil
}
