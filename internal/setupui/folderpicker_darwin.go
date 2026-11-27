//go:build darwin

package setupui

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func pickFolderNative(ctx context.Context) (FolderPickResult, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-e", `POSIX path of (choose folder with prompt "Choose a photo folder for cloudedits")`)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errText := stderr.String()
		if ctx.Err() != nil || strings.Contains(errText, "-128") || strings.Contains(strings.ToLower(errText), "cancel") {
			return FolderPickResult{Supported: true, Cancelled: true}, nil //nolint:nilerr // user cancellation is not an error
		}
		return FolderPickResult{Supported: true}, fmt.Errorf("folder picker failed: %s", strings.TrimSpace(errText))
	}
	picked := strings.TrimSpace(stdout.String())
	if picked == "" {
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	}
	return FolderPickResult{Supported: true, Cancelled: false, Path: filepath.Clean(picked)}, nil
}
