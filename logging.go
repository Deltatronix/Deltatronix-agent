package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"
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

// resolveLogPath picks the active log file: an explicit legacy log_file= wins,
// else <log_dir>/dtx-agent.log, else the default dir.
func resolveLogPath(s LLMSettings) string {
	if s.LogFile != "" {
		return s.LogFile
	}
	if s.LogDir != "" {
		return filepath.Join(s.LogDir, "dtx-agent.log")
	}
	return defaultLogPath()
}

// setupLogging tees the standard logger to a size-rotating file (in addition to
// stderr) so the no-console tray build still has readable logs. When disabled
// via llm.txt, logs go to stderr only. Rotation is handled by lumberjack:
// MaxSize (MB) and MaxBackups map to the log_max_size_mb / log_max_files keys.
func setupLogging(s LLMSettings) {
	if !s.LogEnabled {
		return
	}
	path := resolveLogPath(s)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("log dir %s: %v (logging to stderr only)", filepath.Dir(path), err)
		return
	}
	maxMB := s.LogMaxMB
	if maxMB == 0 {
		maxMB = defaultLogMaxMB
	}
	maxKeep := s.LogMaxKeep
	if maxKeep == 0 {
		maxKeep = defaultLogMaxKeep
	}
	w := &lumberjack.Logger{Filename: path, MaxSize: maxMB, MaxBackups: maxKeep}
	log.SetOutput(io.MultiWriter(os.Stderr, w))
	logMu.Lock()
	resolvedLogPath = path
	logMu.Unlock()
}
