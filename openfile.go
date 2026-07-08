//go:build gui

package main

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
)

// openConsole pops a real terminal window streaming the log file live (tail -f),
// so the no-console GUI build can still show logs as they happen (the Settings
// "Monitor Live Logs" button). Errors are logged, never fatal.
func openConsole(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`tell application "Terminal"
activate
do script "tail -f '%s'"
end tell`, path)
		cmd = exec.Command("osascript", "-e", script)
	case "windows":
		ps := fmt.Sprintf("Get-Content -Path '%s' -Wait -Tail 200", path)
		cmd = exec.Command("cmd", "/c", "start", "", "powershell", "-NoExit", "-Command", ps)
	default:
		for _, term := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xterm"} {
			if _, err := exec.LookPath(term); err != nil {
				continue
			}
			if err := exec.Command(term, "-e", "tail", "-f", path).Start(); err == nil {
				return
			}
		}
		log.Printf("no terminal emulator found; cannot open live console")
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open console: %v", err)
	}
}
