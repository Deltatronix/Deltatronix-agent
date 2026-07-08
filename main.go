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
	case "path":
		if err := cmdPath(args[1:]); err != nil {
			fatal(err)
		}
	case "autostart":
		if err := cmdAutostart(args[1:]); err != nil {
			fatal(err)
		}
	case "":
		// Default: open the GUI (it handles the unpaired case in-window). On a
		// headless build or a display-less machine this runs the connect loop,
		// which requires an already-paired config.
		if err := cmdRun(headless); err != nil {
			fatal(err)
		}
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
  dtx-agent autostart enable [--headless] [--mode xdg|systemd]
  dtx-agent autostart disable
  dtx-agent path add
  dtx-agent path remove
  dtx-agent            (opens the window; runs headless if no display)

Without --headless, run opens a window (Status, Settings, About) and a
system-tray icon (Current Status, Open, Quit). Use --headless (or run with no
display) for servers/systemd/Docker. The autostart and path commands work in
both the GUI and headless builds.
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

	apiURL := strings.TrimRight(*api, "/")
	cfg, err := pairWithCode(context.Background(), apiURL, code)
	if err != nil {
		return err
	}
	path, _ := configPath()
	fmt.Printf("Paired as agent %s. Config saved to %s\n", cfg.AgentID, path)
	fmt.Println("Start the agent with:  dtx-agent run")
	return nil
}

// pairWithCode exchanges a pairing code for an agent token, saves the config,
// and returns it. Shared by the CLI `pair` command and the GUI Status page.
func pairWithCode(ctx context.Context, apiURL, code string) (Config, error) {
	var cfg Config
	if code == "" {
		return cfg, errors.New("pairing code is required")
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
		return cfg, err
	}

	apiURL = strings.TrimRight(apiURL, "/")
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, apiURL+"/compute/agents/pair", bytes.NewReader(body))
	if err != nil {
		return cfg, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "dtx-agent") // satisfies the backend CSRF guard

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cfg, fmt.Errorf("contacting %s: %w", apiURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return cfg, fmt.Errorf("pairing failed (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out struct {
		AgentToken string `json:"agentToken"`
		AgentID    string `json:"agentId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return cfg, fmt.Errorf("decoding pair response: %w", err)
	}
	if out.AgentToken == "" || out.AgentID == "" {
		return cfg, errors.New("pair response missing agentToken/agentId")
	}

	cfg = Config{APIURL: apiURL, AgentID: out.AgentID, AgentToken: out.AgentToken}
	if err := saveConfig(cfg); err != nil {
		return cfg, fmt.Errorf("saving config: %w", err)
	}
	return cfg, nil
}

// revokeAgent best-effort tells the backend to invalidate this agent's token,
// then the caller deletes the local config. Errors are returned for logging but
// must not block local disconnect.
func revokeAgent(ctx context.Context, cfg Config) error {
	if cfg.APIURL == "" || cfg.AgentID == "" {
		return nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := strings.TrimRight(cfg.APIURL, "/") + "/compute/agents/" + cfg.AgentID
	req, err := http.NewRequestWithContext(reqCtx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AgentToken)
	req.Header.Set("X-Requested-With", "dtx-agent")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("revoke failed (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
