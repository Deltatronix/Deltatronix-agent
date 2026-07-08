//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func xdgDesktopPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart", "dtx-agent.desktop"), nil
}

func systemdUnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", "dtx-agent.service"), nil
}

// enableAutostart writes either an XDG autostart .desktop (default, for the GUI)
// or a systemd --user unit, per mode. We remove the other form first so toggling
// modes doesn't leave two autostart entries behind.
func enableAutostart(headless bool, mode string) error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	_ = disableAutostart() // clear any prior entry (either form)

	args := strings.Join(autostartArgs(headless), " ")
	switch mode {
	case autostartSystemd:
		path, err := systemdUnitPath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		unit := fmt.Sprintf(`[Unit]
Description=Deltatronix compute agent
After=network-online.target

[Service]
ExecStart=%s %s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exe, args)
		if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
			return err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return exec.Command("systemctl", "--user", "enable", "--now", "dtx-agent").Run()
	default: // xdg
		path, err := xdgDesktopPath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Deltatronix agent
Exec=%s %s
X-GNOME-Autostart-enabled=true
Terminal=false
`, exe, args)
		return os.WriteFile(path, []byte(desktop), 0o644)
	}
}

func disableAutostart() error {
	// systemd unit
	if path, err := systemdUnitPath(); err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			_ = exec.Command("systemctl", "--user", "disable", "--now", "dtx-agent").Run()
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return rmErr
			}
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		}
	}
	// xdg .desktop
	if path, err := xdgDesktopPath(); err == nil {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
	}
	return nil
}

func autostartEnabled() bool {
	if path, err := xdgDesktopPath(); err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return true
		}
	}
	if path, err := systemdUnitPath(); err == nil {
		if _, statErr := os.Stat(path); statErr == nil {
			return true
		}
	}
	return false
}
