//go:build !windows

package main

import (
	"fmt"
	"os/exec"
	"runtime"
)

// pathTarget is the canonical location dtx-agent is symlinked into. Already on
// PATH everywhere; writing here needs admin.
const pathTarget = "/usr/local/bin/dtx-agent"

// addToPath symlinks the binary into /usr/local/bin with an elevation prompt.
func addToPath() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	return elevate(fmt.Sprintf("ln -sf %q %q", exe, pathTarget),
		fmt.Sprintf("sudo ln -sf %q %q", exe, pathTarget))
}

// removeFromPath removes the symlink (also elevated, since it lives under /usr).
func removeFromPath() error {
	return elevate(fmt.Sprintf("rm -f %q", pathTarget),
		fmt.Sprintf("sudo rm -f %q", pathTarget))
}

// elevate runs a shell command with administrator privileges. macOS uses
// osascript's privilege prompt; Linux uses pkexec. When no graphical elevation
// helper is available (e.g. a container or SSH session), it returns an error
// naming the exact command to run by hand, rather than failing silently.
func elevate(shellCmd, manualHint string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`do shell script %q with administrator privileges`, shellCmd)
		if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
			return fmt.Errorf("elevation failed (%v): run manually: %s", trimOut(out), manualHint)
		}
		return nil
	default: // linux and other unixes
		if _, err := exec.LookPath("pkexec"); err != nil {
			return fmt.Errorf("no graphical elevation available; run manually: %s", manualHint)
		}
		// pkexec needs argv, not a shell string; wrap in sh -c.
		if out, err := exec.Command("pkexec", "sh", "-c", shellCmd).CombinedOutput(); err != nil {
			return fmt.Errorf("elevation failed (%v): run manually: %s", trimOut(out), manualHint)
		}
		return nil
	}
}

func trimOut(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
