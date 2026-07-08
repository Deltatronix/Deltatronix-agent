package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// resolvedLogPath is the log file in use after setupLogging, exposed to the tray
// menu ("Open Logs" / "Open Console"). Guarded because setupLogging runs on the
// startup path while the tray goroutine reads it.
var (
	logMu           sync.Mutex
	resolvedLogPath string
)

// defaultLogPath is <config dir>/dtx-agent.log, alongside config.json and llm.txt.
func defaultLogPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "dtx-agent.log"
	}
	return filepath.Join(dir, "dtx-agent", "dtx-agent.log")
}

// logPath returns the log file the tray items should open. Falls back to the
// default before setupLogging has run.
func logPath() string {
	logMu.Lock()
	defer logMu.Unlock()
	if resolvedLogPath != "" {
		return resolvedLogPath
	}
	return defaultLogPath()
}

// setupLogging tees the standard logger to a file (in addition to stderr) so the
// no-console tray build still has readable logs. When disabled via llm.txt, logs
// go to stderr only. ponytail: single append file, no rotation — add if it grows.
func setupLogging(s LLMSettings) {
	if !s.LogEnabled {
		return
	}
	path := s.LogFile
	if path == "" {
		path = defaultLogPath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("log dir %s: %v (logging to stderr only)", filepath.Dir(path), err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("open log %s: %v (logging to stderr only)", path, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	logMu.Lock()
	resolvedLogPath = path
	logMu.Unlock()
}
