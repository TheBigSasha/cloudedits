package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	setupAppWindowSizeFlag = "--window-size=780,860"
	guiAddrFilename        = "gui.addr"
)

// openAppWindowCommand chooses a desktop app-window command (preferring
// Edge/Chrome --app=<url> --window-size=780,860 using the user's default
// browser profile so Tailscale and hdr.toys sessions carry over) and falls
// back to openBrowserCommand when no supported Chromium browser is found.
func openAppWindowCommand(
	goos, rawURL string,
	hasDisplay bool,
	getenv func(string) string,
	lookPath func(string) (string, error),
	fileExists func(string) bool,
) (string, []string, bool) {
	appArgs := []string{"--app=" + rawURL, setupAppWindowSizeFlag}
	switch goos {
	case "windows":
		var candidates []string
		for _, envKey := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			if base := strings.TrimSpace(getenv(envKey)); base != "" {
				candidates = append(candidates, filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"))
			}
		}
		for _, c := range candidates {
			if fileExists(c) {
				return c, appArgs, true
			}
		}
		for _, bin := range []string{"msedge.exe", "msedge"} {
			if p, err := lookPath(bin); err == nil && p != "" {
				return p, appArgs, true
			}
		}
		var chromeCandidates []string
		for _, envKey := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if base := strings.TrimSpace(getenv(envKey)); base != "" {
				chromeCandidates = append(chromeCandidates, filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"))
			}
		}
		for _, c := range chromeCandidates {
			if fileExists(c) {
				return c, appArgs, true
			}
		}
		for _, bin := range []string{"chrome.exe", "chrome"} {
			if p, err := lookPath(bin); err == nil && p != "" {
				return p, appArgs, true
			}
		}
		return openBrowserCommand(goos, rawURL, hasDisplay)

	case "darwin":
		for _, c := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser",
		} {
			if fileExists(c) {
				return c, appArgs, true
			}
		}
		return openBrowserCommand(goos, rawURL, hasDisplay)

	default:
		if !hasDisplay {
			return "", nil, false
		}
		for _, bin := range []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium",
			"chromium-browser",
			"microsoft-edge",
			"brave-browser",
		} {
			if p, err := lookPath(bin); err == nil && p != "" {
				return p, appArgs, true
			}
		}
		return openBrowserCommand(goos, rawURL, hasDisplay)
	}
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

var openSetupWindow = openAppWindow

func openAppWindow(rawURL string) error {
	hasDisplay := os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	name, args, ok := openAppWindowCommand(runtime.GOOS, rawURL, hasDisplay, os.Getenv, exec.LookPath, isRegularFile)
	if !ok {
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.Env = withoutSecrets(os.Environ())
	configureAppLaunchCmd(cmd)
	if err := cmd.Start(); err != nil {
		return openBrowser(rawURL)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// probeActiveGUI checks <dataDir>/gui.addr for an already-running cloudedits
// setup wizard on loopback and returns its setup URL if it responds as a live
// setup UI instance.
func probeActiveGUI(ctx context.Context, dataDir string) (string, bool) {
	if dataDir == "" {
		return "", false
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, guiAddrFilename))
	if err != nil {
		return "", false
	}
	addr := strings.TrimSpace(string(raw))
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", false
	}
	ip := net.ParseIP(host)
	if (ip == nil || !ip.IsLoopback()) && !strings.EqualFold(host, "localhost") {
		return "", false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+addr+"/api/state", nil)
	if err != nil {
		return "", false
	}
	resp, err := (&http.Client{Timeout: 600 * time.Millisecond}).Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", false
	}
	if _, ok := payload["folders"]; !ok {
		return "", false
	}
	return fmt.Sprintf("http://%s/setup", addr), true
}

func writeGUIAddr(dataDir, addr string) {
	if dataDir == "" || addr == "" {
		return
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dataDir, guiAddrFilename), []byte(addr+"\n"), 0o600)
}

func removeGUIAddrIfMatch(dataDir, addr string) {
	if dataDir == "" || addr == "" {
		return
	}
	p := filepath.Join(dataDir, guiAddrFilename)
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	if strings.TrimSpace(string(raw)) == addr {
		_ = os.Remove(p)
	}
}
