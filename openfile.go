package main

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
)

// openInEditor opens path in the OS default handler (a text editor for llm.txt /
// the log file). Errors are logged, not fatal — a tray click should never crash.
func openInEditor(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		// The empty first arg is start's window-title placeholder.
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open %s: %v", path, err)
	}
}

// showAbout pops a native OS message dialog with the app name and version.
// Reuses the shell-out approach (no GUI toolkit needed); text is fixed, not user
// input, so simple single-quoting is safe. Errors are logged, never fatal.
func showAbout() {
	title := appName
	body := fmt.Sprintf("%s\nVersion %s", appName, version)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display dialog "%s" with title "%s" buttons {"OK"} default button "OK" with icon note`, body, title)
		cmd = exec.Command("osascript", "-e", script)
	case "windows":
		ps := fmt.Sprintf(`Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.MessageBox]::Show('%s','%s')`, body, title)
		cmd = exec.Command("powershell", "-NoProfile", "-Command", ps)
	default:
		// zenity is the common GTK dialog tool; if absent, fall back to a log line.
		if _, err := exec.LookPath("zenity"); err != nil {
			log.Printf("about: %s", body)
			return
		}
		cmd = exec.Command("zenity", "--info", "--title", title, "--text", body)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("show about: %v", err)
	}
}

// openConsole pops a real terminal window streaming the log file live (tail -f),
// so the no-console tray build can still show logs as they happen. Falls back to
// opening the log file in an editor if no terminal is available.
func openConsole(path string) {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`tell application "Terminal"
activate
do script "tail -f '%s'"
end tell`, path)
		if err := exec.Command("osascript", "-e", script).Start(); err != nil {
			log.Printf("open console: %v", err)
			openInEditor(path)
		}
	case "windows":
		ps := fmt.Sprintf("Get-Content -Path '%s' -Wait -Tail 200", path)
		if err := exec.Command("cmd", "/c", "start", "", "powershell", "-NoExit", "-Command", ps).Start(); err != nil {
			log.Printf("open console: %v", err)
			openInEditor(path)
		}
	default:
		for _, term := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xterm"} {
			if _, err := exec.LookPath(term); err != nil {
				continue
			}
			if err := exec.Command(term, "-e", "tail", "-f", path).Start(); err == nil {
				return
			}
		}
		log.Printf("no terminal emulator found; opening log file instead")
		openInEditor(path)
	}
}
