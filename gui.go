//go:build gui

package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// pageTitle renders a page header: heading-sized, bold, and in a muted color so
// it reads as a title and doesn't blend into the body text.
func pageTitle(text string) *canvas.Text {
	t := canvas.NewText(text, theme.Color(theme.ColorNamePlaceHolder))
	t.TextSize = theme.TextHeadingSize()
	t.TextStyle = fyne.TextStyle{Bold: true}
	return t
}

// statusPill wraps a status label in a rounded, outlined rectangle so it reads
// as a pill. The trailing spacer keeps the pill hugging its content on the left
// instead of stretching the full width.
func statusPill(o fyne.CanvasObject) fyne.CanvasObject {
	rect := canvas.NewRectangle(color.Transparent)
	rect.CornerRadius = 14
	rect.StrokeColor = theme.Color(theme.ColorNamePlaceHolder)
	rect.StrokeWidth = 1
	pill := container.NewStack(rect, container.NewPadded(o))
	return container.NewHBox(pill, layout.NewSpacer())
}

//go:embed resources/tray/icon.png
var iconPNG []byte

// trayIcon is the Fyne resource form of the embedded tray/app icon.
func trayIcon() fyne.Resource { return fyne.NewStaticResource("icon.png", iconPNG) }

// runGUI builds the window (Status / Settings / About), wires the system tray,
// and owns the connect-loop supervisor so pairing/disconnect take effect live.
// Blocks until the user picks Quit.
func runGUI() error {
	a := app.NewWithID("io.deltatronix.agent")
	a.SetIcon(trayIcon())

	w := a.NewWindow(appName)
	w.Resize(fyne.NewSize(660, 480))
	w.SetCloseIntercept(func() { w.Hide() }) // close = hide to tray, not quit

	sup := &supervisor{}

	content := container.NewStack()
	showPage := func(o fyne.CanvasObject) {
		content.Objects = []fyne.CanvasObject{o}
		content.Refresh()
	}

	statusPage, refreshStatus := buildStatusPage(a, w, sup)
	settingsPage := buildSettingsPage(a, w)
	aboutPage := buildAboutPage()

	nav := container.NewVBox(
		widget.NewButtonWithIcon("Status", nil, func() { refreshStatus(); showPage(statusPage) }),
		widget.NewButtonWithIcon("Settings", nil, func() { showPage(settingsPage) }),
		widget.NewButtonWithIcon("About", nil, func() { showPage(aboutPage) }),
	)
	w.SetContent(container.NewPadded(container.NewBorder(nil, nil, nav, nil, content)))
	showPage(statusPage)

	setupTray(a, w)

	// Start the loop if already paired, and decide initial visibility:
	// unpaired → open the window to pair; paired → start hidden in the tray.
	if cfg, err := loadConfig(); err == nil {
		sup.start(cfg)
	} else {
		w.Show()
	}
	refreshStatus()

	a.Run()
	return nil
}

// setupTray installs the system-tray icon + menu (Current Status, Open, Quit)
// and keeps the status line in sync with the connect loop.
func setupTray(a fyne.App, w fyne.Window) {
	desk, ok := a.(desktop.App)
	if !ok {
		return // no tray on this driver; window-only
	}
	statusItem := fyne.NewMenuItem("Status: "+statusNotPaired, nil)
	statusItem.Disabled = true
	menu := fyne.NewMenu(appName,
		statusItem,
		fyne.NewMenuItem("Open", func() { w.Show(); w.RequestFocus() }),
	)
	desk.SetSystemTrayMenu(menu)
	desk.SetSystemTrayIcon(trayIcon())

	onStatus(func(s string) {
		fyne.Do(func() {
			statusItem.Label = "Status: " + s
			desk.SetSystemTrayMenu(menu) // re-set to refresh the rendered label
		})
	})
}

// buildStatusPage shows platform + LLM-backend health and the pairing /
// disconnect controls. The returned refresh func re-reads the paired state and
// flips which controls are visible.
func buildStatusPage(a fyne.App, w fyne.Window, sup *supervisor) (fyne.CanvasObject, func()) {
	platform := widget.NewLabel("")
	platform.TextStyle = fyne.TextStyle{Bold: true}
	platformHelp := widget.NewLabel("Connection to the Deltatronix platform.")
	platformHelp.Wrapping = fyne.TextWrapWord

	llm := widget.NewLabel("Checking local LLM backend…")
	llmHelp := widget.NewLabel("Your local OpenAI-compatible server (Ollama / LM Studio).")
	llmHelp.Wrapping = fyne.TextWrapWord

	// Pairing controls (unpaired only).
	code := widget.NewEntry()
	code.SetPlaceHolder("Pairing code from the Deltatronix web app")
	pairBtn := widget.NewButton("Pair", nil)
	pairMsg := widget.NewLabel("")
	pairMsg.Wrapping = fyne.TextWrapWord
	pairBox := container.NewVBox(
		widget.NewLabel("Enter your pairing code to connect this machine:"),
		code, pairBtn, pairMsg,
	)

	// Disconnect control (paired only).
	disconnectBtn := widget.NewButton("Disconnect", nil)
	disconnectBox := container.NewVBox(
		widget.NewLabel("This machine is paired. Disconnect to unpair and connect a different profile."),
		disconnectBtn,
	)

	// refresh flips visibility based on paired state.
	refresh := func() {
		_, err := loadConfig()
		paired := err == nil
		if paired {
			pairBox.Hide()
			disconnectBox.Show()
		} else {
			pairBox.Show()
			disconnectBox.Hide()
			platform.SetText(statusNotPaired)
		}
	}

	pairBtn.OnTapped = func() {
		pairMsg.SetText("Pairing…")
		pairBtn.Disable()
		go func() {
			cfg, err := pairWithCode(context.Background(), defaultAPI, code.Text)
			fyne.Do(func() {
				pairBtn.Enable()
				if err != nil {
					pairMsg.SetText("Pairing failed: " + err.Error())
					return
				}
				pairMsg.SetText("")
				code.SetText("")
				sup.start(cfg)
				refresh()
			})
		}()
	}

	disconnectBtn.OnTapped = func() {
		dialog.ShowConfirm("Disconnect", "Unpair this machine from the platform?", func(ok bool) {
			if !ok {
				return
			}
			disconnectBtn.Disable()
			go func() {
				cfg, _ := loadConfig()
				if err := revokeAgent(context.Background(), cfg); err != nil {
					log.Printf("revoke (continuing to local disconnect): %v", err)
				}
				sup.stop()
				if err := deleteConfig(); err != nil {
					log.Printf("delete config: %v", err)
				}
				fyne.Do(func() {
					disconnectBtn.Enable()
					refresh()
				})
			}()
		}, w)
	}

	// Live platform status from the connect loop.
	onStatus(func(s string) { fyne.Do(func() { platform.SetText(s) }) })

	// Periodically probe the LLM backend for the health line.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		update := func() {
			models, err := probeModels(context.Background())
			fyne.Do(func() {
				if err != nil {
					llm.SetText("Not reachable at " + llmBase)
				} else {
					llm.SetText(fmt.Sprintf("Reachable at %s — %d model(s) loaded", llmBase, len(models)))
				}
			})
		}
		update()
		for range t.C {
			update()
		}
	}()

	page := container.NewVBox(
		pageTitle("Status"),
		widget.NewLabel("Platform"), statusPill(platform), platformHelp,
		pairBox, disconnectBox,
		widget.NewSeparator(),
		widget.NewLabel("Local LLM backend"), statusPill(llm), llmHelp,
	)
	return container.NewScroll(page), refresh
}

// buildSettingsPage edits the LLM backend + logging settings (persisted to
// llm.txt) and hosts the PATH / autostart / live-logs actions.
func buildSettingsPage(a fyne.App, w fyne.Window) fyne.CanvasObject {
	s := loadLLMSettings()

	backend := widget.NewEntry()
	backend.SetText(s.BackendURL)
	ollamaBtn := widget.NewButton("Ollama", func() { backend.SetText("http://localhost:11434/v1") })
	lmBtn := widget.NewButton("LM Studio", func() { backend.SetText("http://localhost:1234/v1") })
	backendHelp := widget.NewLabel("Local LLM backend (OpenAI-compatible API).")
	backendHelp.Wrapping = fyne.TextWrapWord

	logsChk := widget.NewCheck("Save logs to a file", nil)
	logsChk.SetChecked(s.LogEnabled)

	logDir := widget.NewEntry()
	logDir.SetText(s.LogDir)
	logDir.SetPlaceHolder("Default: config folder")
	browseBtn := widget.NewButton("Choose folder…", func() {
		dialog.ShowFolderOpen(func(u fyne.ListableURI, err error) {
			if err == nil && u != nil {
				logDir.SetText(u.Path())
			}
		}, w)
	})
	maxSize := widget.NewEntry()
	maxSize.SetText(strconv.Itoa(nonZero(s.LogMaxMB, defaultLogMaxMB)))
	maxFiles := widget.NewEntry()
	maxFiles.SetText(strconv.Itoa(nonZero(s.LogMaxKeep, defaultLogMaxKeep)))

	logDetails := container.NewVBox(
		container.NewBorder(nil, nil, nil, browseBtn, logDir),
		container.NewGridWithColumns(2,
			widget.NewLabel("Max size per file (MB)"), maxSize,
			widget.NewLabel("Number of files to keep"), maxFiles,
		),
	)
	syncLogDetails := func() {
		if logsChk.Checked {
			logDetails.Show()
		} else {
			logDetails.Hide()
		}
	}
	logsChk.OnChanged = func(bool) { syncLogDetails() }
	syncLogDetails()

	monitorBtn := widget.NewButton("Monitor Live Logs", func() { openConsole(logPath()) })

	// Save writes llm.txt then relaunches so the new backend/logging settings
	// take effect (they are read once at startup).
	saveBtn := widget.NewButton("Save changes (restart)", func() {
		ns := LLMSettings{
			BackendURL: backend.Text,
			LogEnabled: logsChk.Checked,
			LogDir:     logDir.Text,
			LogMaxMB:   atoiSafe(maxSize.Text),
			LogMaxKeep: atoiSafe(maxFiles.Text),
		}
		if err := writeLLMSettings(ns); err != nil {
			dialog.ShowError(err, w)
			return
		}
		dialog.ShowConfirm("Restart", "Restart now to apply the changes?", func(ok bool) {
			if ok {
				relaunch(a)
			}
		}, w)
	})
	saveBtn.Importance = widget.HighImportance

	pathBtn := widget.NewButton("Add to PATH", func() {
		go func() {
			err := addToPath()
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, w)
				} else {
					dialog.ShowInformation("PATH", "dtx-agent added to your PATH.", w)
				}
			})
		}()
	})

	autoChk := widget.NewCheck("Start automatically on login", nil)
	autoChk.SetChecked(autostartEnabled())
	modeSelect := widget.NewSelect([]string{autostartXDG, autostartSystemd}, nil)
	modeSelect.SetSelected(autostartXDG)
	autoChk.OnChanged = func(on bool) {
		go func() {
			var err error
			if on {
				err = enableAutostart(false, modeSelect.Selected)
			} else {
				err = disableAutostart()
			}
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, w)
					autoChk.SetChecked(autostartEnabled())
				}
			})
		}()
	}
	autostartRow := fyne.CanvasObject(autoChk)
	if runtime.GOOS == "linux" {
		autostartRow = container.NewVBox(autoChk, container.NewHBox(widget.NewLabel("Method:"), modeSelect))
	}

	page := container.NewVBox(
		pageTitle("Settings"),
		widget.NewLabel("Backend URL"),
		container.NewBorder(nil, nil, nil, container.NewHBox(ollamaBtn, lmBtn), backend),
		backendHelp,
		widget.NewSeparator(),
		monitorBtn,
		logsChk, logDetails,
		widget.NewSeparator(),
		pathBtn,
		widget.NewSeparator(),
		autostartRow,
		widget.NewSeparator(),
		saveBtn,
	)
	return container.NewScroll(page)
}

func buildAboutPage() fyne.CanvasObject {
	desc := widget.NewLabel("Deltatronix agent runs local AI vision jobs on your machine. " +
		"It pairs with the Deltatronix platform, connects over an outbound WebSocket, " +
		"and processes jobs one at a time using your local OpenAI-compatible LLM server.")
	desc.Wrapping = fyne.TextWrapWord
	return container.NewVBox(
		pageTitle(appName),
		widget.NewLabel("Version "+version),
		widget.NewSeparator(),
		desc,
	)
}

// relaunch starts a fresh instance and quits this one, so restart-required
// settings take effect with one click.
func relaunch(a fyne.App) {
	exe, err := selfPath()
	if err != nil {
		log.Printf("relaunch: %v", err)
		a.Quit()
		return
	}
	if err := exec.Command(exe, "run").Start(); err != nil {
		log.Printf("relaunch: %v", err)
	}
	a.Quit()
}

func nonZero(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
