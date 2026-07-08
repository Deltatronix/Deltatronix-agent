package main

import (
	"errors"
	"flag"
	"fmt"
)

// Autostart modes matter only on Linux (XDG .desktop vs systemd --user). Other
// OSes ignore the mode.
const (
	autostartXDG     = "xdg"
	autostartSystemd = "systemd"
)

// cmdAutostart implements `dtx-agent autostart enable|disable`, the
// headless-accessible entry point; the GUI toggle calls
// enableAutostart/disableAutostart directly. Per-OS implementations live in
// autostart_{darwin,windows,linux}.go.
func cmdAutostart(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	fs := flag.NewFlagSet("autostart", flag.ContinueOnError)
	headless := fs.Bool("headless", false, "install a unit that launches `run --headless`")
	mode := fs.String("mode", autostartXDG, "linux only: xdg | systemd")
	if len(args) > 1 {
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
	}

	switch sub {
	case "enable":
		if err := enableAutostart(*headless, *mode); err != nil {
			return err
		}
		fmt.Println("Autostart enabled.")
		return nil
	case "disable":
		if err := disableAutostart(); err != nil {
			return err
		}
		fmt.Println("Autostart disabled.")
		return nil
	default:
		return errors.New("usage: dtx-agent autostart enable [--headless] [--mode xdg|systemd] | disable")
	}
}

// autostartArgs is the argument list the installed unit launches with: `run`
// for the GUI, `run --headless` for the daemon.
func autostartArgs(headless bool) []string {
	if headless {
		return []string{"run", "--headless"}
	}
	return []string{"run"}
}
