package main

import (
	_ "embed"
	"log"
	"runtime"
	"sync"

	"fyne.io/systray"
)

//go:embed resources/tray/icon.png
var iconPNG []byte

//go:embed resources/tray/icon_template.png
var iconTemplatePNG []byte

//go:embed resources/tray/icon.ico
var iconICO []byte

// statusItem is the disabled menu line showing connection state. nil when running
// headless, so setStatus is a no-op there.
var (
	statusMu   sync.Mutex
	statusItem *systray.MenuItem
)

// setStatus updates the tray status line. Safe to call from any goroutine and
// when no tray is running.
func setStatus(s string) {
	statusMu.Lock()
	item := statusItem
	statusMu.Unlock()
	if item != nil {
		item.SetTitle(s)
	}
}

// runTray shows the system-tray icon and menu, runs the agent connect loop in a
// goroutine, and blocks until the user picks Quit.
func runTray(cfg Config) error {
	systray.Run(func() { onReady(cfg) }, nil)
	return nil
}

func onReady(cfg Config) {
	switch runtime.GOOS {
	case "darwin":
		systray.SetTemplateIcon(iconTemplatePNG, iconPNG)
	case "windows":
		systray.SetIcon(iconICO)
	default:
		systray.SetIcon(iconPNG)
	}
	systray.SetTooltip(appName)

	// Menu, top→bottom, no separators (status line first).
	status := systray.AddMenuItem("Starting…", "Connection status")
	status.Disable()
	statusMu.Lock()
	statusItem = status
	statusMu.Unlock()

	params := systray.AddMenuItem("Parameters File", "Edit the LLM backend settings (llm.txt)")
	console := systray.AddMenuItem("Open Console", "Stream logs live in a terminal")
	logs := systray.AddMenuItem("Open Logs", "Open the log file")
	about := systray.AddMenuItem("About", "Show the app name and version")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "Stop the agent and exit")

	go connectForever(cfg)

	go func() {
		for {
			select {
			case <-params.ClickedCh:
				if path, err := llmConfigPath(); err == nil {
					openInEditor(path)
				} else {
					log.Printf("locate llm.txt: %v", err)
				}
			case <-console.ClickedCh:
				openConsole(logPath())
			case <-logs.ClickedCh:
				openInEditor(logPath())
			case <-about.ClickedCh:
				showAbout()
			case <-quit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}
