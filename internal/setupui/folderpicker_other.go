//go:build !windows && !darwin

package setupui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func pickFolderNative(ctx context.Context) (FolderPickResult, error) {
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return FolderPickResult{Supported: false}, nil
	}
	if zenity, err := exec.LookPath("zenity"); err == nil {
		return runLinuxPicker(ctx, zenity, "--file-selection", "--directory", "--title=Choose a photo folder for cloudedits")
	}
	if kdialog, err := exec.LookPath("kdialog"); err == nil {
		home, _ := os.UserHomeDir()
		if home == "" {
			home = "/"
		}
		return runLinuxPicker(ctx, kdialog, "--getexistingdirectory", home, "--title", "Choose a photo folder for cloudedits")
	}
	return FolderPickResult{Supported: false}, nil
}

func runLinuxPicker(ctx context.Context, bin string, args ...string) (FolderPickResult, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ctx.Err() != nil || (errors.As(err, &exitErr) && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 5)) {
			return FolderPickResult{Supported: true, Cancelled: true}, nil //nolint:nilerr // user cancellation is not an error
		}
		return FolderPickResult{Supported: true}, fmt.Errorf("folder picker failed: %s", strings.TrimSpace(stderr.String()))
	}
	picked := strings.TrimSpace(stdout.String())
	if picked == "" {
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	}
	return FolderPickResult{Supported: true, Cancelled: false, Path: filepath.Clean(picked)}, nil
}
