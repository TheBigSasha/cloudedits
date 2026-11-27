//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

func launchHiddenGUI(exePath string, args []string) error {
	cmd := exec.Command(exePath, args...)
	return cmd.Start()
}

func reportLaunchError(title, message string) {
	fmt.Fprintf(os.Stderr, "%s: %s\n", title, message)
}
