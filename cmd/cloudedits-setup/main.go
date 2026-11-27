// Command cloudedits-setup is a zero-console launcher for the cloudedits setup
// UI. On Windows it is built with -H=windowsgui and spawns the sibling
// cloudedits.exe with CREATE_NO_WINDOW so shortcuts and installer launches
// never flash a console window.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

var version = "dev"

func resolveCloudeditsBinary(selfPath, goos string, stat func(string) (os.FileInfo, error), lookPath func(string) (string, error)) (string, error) {
	binName := "cloudedits"
	if goos == "windows" {
		binName = "cloudedits.exe"
	}
	if selfPath != "" {
		sibling := filepath.Join(filepath.Dir(selfPath), binName)
		if info, err := stat(sibling); err == nil && !info.IsDir() {
			return sibling, nil
		}
	}
	if p, err := lookPath(binName); err == nil && p != "" {
		return p, nil
	}
	return "", fmt.Errorf("could not locate %s next to %s or on PATH", binName, selfPath)
}

func main() {
	self, _ := os.Executable()
	target, err := resolveCloudeditsBinary(self, runtime.GOOS, os.Stat, exec.LookPath)
	if err != nil {
		reportLaunchError("cloudedits Setup", err.Error())
		os.Exit(1)
	}
	args := append([]string{"gui"}, os.Args[1:]...)
	if err := launchHiddenGUI(target, args); err != nil {
		reportLaunchError("cloudedits Setup", fmt.Sprintf("Failed to launch cloudedits Setup (%s): %v", version, err))
		os.Exit(1)
	}
}
