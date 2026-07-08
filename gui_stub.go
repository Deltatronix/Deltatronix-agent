//go:build !gui

package main

import "errors"

// runGUI is the headless build's stand-in: there is no window, so tell the user
// to run the daemon explicitly. Reached only if someone runs the headless binary
// interactively on a machine with a display.
func runGUI() error {
	return errors.New("this build has no GUI; run `dtx-agent run --headless`")
}
