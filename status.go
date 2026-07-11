package main

import "sync"

// Connection-status strings, shared by the tray line and the GUI Status page so
// both show the same wording.
const (
	statusNotPaired     = "Not paired"
	statusConnecting    = "Connecting…"
	statusConnected     = "Connected"
	statusReconnecting  = "Reconnecting…"
	statusNoLLM         = "No LLM backend"
	statusNoVisionModel = "Connected — no vision model"
)

// Status is broadcast from the connect loop to any observers (the tray menu
// line, the GUI Status page). In the headless build there are no observers, so
// setStatus just records the latest value — a no-op cost.
var (
	statusMu       sync.Mutex
	statusCurrent  = statusNotPaired
	statusWatchers []func(string)
)

// setStatus records the latest status and notifies observers. Safe from any
// goroutine.
func setStatus(s string) {
	statusMu.Lock()
	statusCurrent = s
	watchers := append([]func(string){}, statusWatchers...)
	statusMu.Unlock()
	for _, w := range watchers {
		w(s)
	}
}

// onStatus registers an observer and immediately delivers the current value, so
// a late subscriber (e.g. the GUI opening) starts in sync.
func onStatus(fn func(string)) {
	statusMu.Lock()
	statusWatchers = append(statusWatchers, fn)
	cur := statusCurrent
	statusMu.Unlock()
	fn(cur)
}
