package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const defaultAPI = "https://api.deltatronix.io"

func main() {
	log.SetFlags(log.LstdFlags)

	// --headless forces the no-GUI daemon; strip it so it works before or after
	// the subcommand (`dtx-agent run --headless`, `dtx-agent --headless`).
	headless := false
	var args []string
	for _, a := range os.Args[1:] {
		if a == "--headless" {
			headless = true
			continue
		}
		args = append(args, a)
	}
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}

	switch cmd {
	case "pair":
		if err := cmdPair(args[1:]); err != nil {
			fatal(err)
		}
	case "run":
		if err := cmdRun(headless); err != nil {
			fatal(err)
		}
	case "":
		// Default: run if paired, else show usage.
		if _, err := loadConfig(); err == nil {
			if err := cmdRun(headless); err != nil {
				fatal(err)
			}
			return
		}
		usage()
		os.Exit(2)
	default:
		usage()
		os.Exit(2)
	}
}

// noDisplay reports whether there's no GUI session, so the tray build falls back
// to headless instead of crashing. Only Linux can plausibly run without one; on
// macOS/Windows a desktop session is assumed.
func noDisplay() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

func usage() {
	fmt.Fprint(os.Stderr, `dtx-agent — Deltatronix local compute agent

Usage:
  dtx-agent pair <code> [--api https://api.deltatronix.io]
  dtx-agent run [--headless]
  dtx-agent            (runs if already paired; shows a system-tray icon)

Without --headless, run shows a system-tray icon with a menu (Parameters File,
Open Console, Open Logs, Quit). Use --headless (or run with no display) for
servers/systemd.
`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dtx-agent: "+err.Error())
	os.Exit(1)
}

// cmdPair exchanges a pairing code for a long-lived agent token and stores it.
func cmdPair(args []string) error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	api := fs.String("api", defaultAPI, "backend API base URL")

	// Accept the code either before or after flags.
	var code string
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		code, rest = args[0], args[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if code == "" {
		code = fs.Arg(0)
	}
	if code == "" {
		return errors.New("usage: dtx-agent pair <code> [--api URL]")
	}

	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "dtx-agent"
	}
	body, err := json.Marshal(map[string]string{
		"code":     code,
		"name":     name,
		"platform": runtime.GOOS + "/" + runtime.GOARCH,
	})
	if err != nil {
		return err
	}

	apiURL := strings.TrimRight(*api, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/compute/agents/pair", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "dtx-agent") // satisfies the backend CSRF guard

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", apiURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("pairing failed (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		AgentToken string `json:"agentToken"`
		AgentID    string `json:"agentId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decoding pair response: %w", err)
	}
	if out.AgentToken == "" || out.AgentID == "" {
		return errors.New("pair response missing agentToken/agentId")
	}

	if err := saveConfig(Config{APIURL: apiURL, AgentID: out.AgentID, AgentToken: out.AgentToken}); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	path, _ := configPath()
	fmt.Printf("Paired as agent %s (%s). Config saved to %s\n", out.AgentID, name, path)
	fmt.Println("Start the agent with:  dtx-agent run")
	return nil
}
