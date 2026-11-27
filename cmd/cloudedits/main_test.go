package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/setupui"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
	}()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

func TestCLISubcommands(t *testing.T) {
	dataDir := t.TempDir()
	photoDir := t.TempDir()
	dockerDir := filepath.Join(t.TempDir(), "docker")

	if code := run([]string{"setup", "--folder", photoDir, "--hostname", "mylib", "--data-dir", dataDir, "--json"}); code != exitOK {
		t.Fatalf("setup exited %d", code)
	}
	if code := run([]string{"status", "--data-dir", dataDir, "--json"}); code != exitOK {
		t.Fatalf("status exited %d", code)
	}
	// A library that has not joined a tailnet and is not serving needs
	// action from the user.
	if code := run([]string{"doctor", "--data-dir", dataDir, "--json"}); code != exitActionRequired {
		t.Fatalf("doctor exited %d, want %d", code, exitActionRequired)
	}
	if code := run([]string{"folders", "list", "--data-dir", dataDir, "--json"}); code != exitOK {
		t.Fatalf("folders list exited %d", code)
	}
	if code := run([]string{"qr", "--url", "https://mylib.example.ts.net", "--json"}); code != exitOK {
		t.Fatalf("qr exited %d", code)
	}
	if code := run([]string{"docker-init", "--folder", photoDir, "--out", dockerDir, "--json"}); code != exitOK {
		t.Fatalf("docker-init exited %d", code)
	}
	if code := run([]string{"version"}); code != exitOK {
		t.Fatalf("version exited %d", code)
	}
	if code := run([]string{"unknown-subcmd"}); code != exitUsage {
		t.Fatalf("expected exitUsage=2 on unknown subcommand, got %d", code)
	}
}

func TestVersionCommand(t *testing.T) {
	t.Run("plain text", func(t *testing.T) {
		var code int
		out := captureStdout(t, func() {
			code = run([]string{"version"})
		})
		if code != exitOK {
			t.Fatalf("version exited %d", code)
		}
		want := "cloudedits " + version
		if strings.TrimSpace(out) != want {
			t.Fatalf("version output = %q, want %q", strings.TrimSpace(out), want)
		}
	})

	t.Run("json output", func(t *testing.T) {
		for _, cmd := range []string{"version", "--version", "-v"} {
			var code int
			out := captureStdout(t, func() {
				code = run([]string{cmd, "--json"})
			})
			if code != exitOK {
				t.Fatalf("%s --json exited %d", cmd, code)
			}
			var got versionInfo
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("%s --json invalid JSON: %v\noutput: %s", cmd, err, out)
			}
			if got.Version != version {
				t.Fatalf("%s --json version = %q, want %q", cmd, got.Version, version)
			}
			if got.OS != runtime.GOOS {
				t.Fatalf("%s --json os = %q, want %q", cmd, got.OS, runtime.GOOS)
			}
			if got.Arch != runtime.GOARCH {
				t.Fatalf("%s --json arch = %q, want %q", cmd, got.Arch, runtime.GOARCH)
			}
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		if code := run([]string{"version", "--not-a-flag"}); code != exitUsage {
			t.Fatalf("expected exitUsage=%d, got %d", exitUsage, code)
		}
	})
}

func TestOpenBrowserCommand(t *testing.T) {
	url := "http://127.0.0.1:43210/setup"

	name, args, ok := openBrowserCommand("darwin", url, false)
	if !ok || name != "open" || len(args) != 1 || args[0] != url {
		t.Fatalf("darwin: got (%q, %v, %v)", name, args, ok)
	}

	name, args, ok = openBrowserCommand("windows", url, false)
	if !ok || name != "rundll32" || len(args) != 2 || args[1] != url {
		t.Fatalf("windows: got (%q, %v, %v)", name, args, ok)
	}

	_, _, ok = openBrowserCommand("linux", url, false)
	if ok {
		t.Fatalf("linux headless should not attempt to launch xdg-open")
	}

	name, args, ok = openBrowserCommand("linux", url, true)
	if !ok || name != "xdg-open" || len(args) != 1 || args[0] != url {
		t.Fatalf("linux desktop: got (%q, %v, %v)", name, args, ok)
	}
}

func TestNoArgsAutoLaunchesGUIOnlyWhenLaunchedFromDesktop(t *testing.T) {
	origCheck := isLaunchedFromDesktop
	origRun := runGUIFromDesktop
	t.Cleanup(func() {
		isLaunchedFromDesktop = origCheck
		runGUIFromDesktop = origRun
	})

	guiCalls := 0
	runGUIFromDesktop = func() int {
		guiCalls++
		return exitOK
	}

	// Normal terminal invocation prints usage and returns exitUsage (2).
	isLaunchedFromDesktop = func() bool { return false }
	if code := run(nil); code != exitUsage {
		t.Fatalf("no-args from terminal: got exit %d, want %d", code, exitUsage)
	}
	if guiCalls != 0 {
		t.Fatalf("terminal no-args unexpectedly launched GUI (%d times)", guiCalls)
	}

	// Direct desktop double-click launches the GUI cleanly.
	isLaunchedFromDesktop = func() bool { return true }
	if code := run(nil); code != exitOK {
		t.Fatalf("no-args from desktop: got exit %d, want %d", code, exitOK)
	}
	if guiCalls != 1 {
		t.Fatalf("desktop no-args guiCalls = %d, want 1", guiCalls)
	}
}

func TestOpenAppWindowCommandPrefersStandaloneAppMode(t *testing.T) {
	url := "http://127.0.0.1:43210/setup"
	noLook := func(string) (string, error) { return "", errors.New("not found") }
	noEnv := func(string) string { return "" }
	noFile := func(string) bool { return false }

	t.Run("windows prefers msedge app window", func(t *testing.T) {
		edgePath := filepath.Join(`C:\Program Files (x86)`, "Microsoft", "Edge", "Application", "msedge.exe")
		name, args, ok := openAppWindowCommand(
			"windows", url, false,
			func(k string) string {
				if k == "ProgramFiles(x86)" {
					return `C:\Program Files (x86)`
				}
				return ""
			},
			noLook,
			func(p string) bool { return p == edgePath },
		)
		if !ok || name != edgePath || len(args) != 2 || args[0] != "--app="+url || args[1] != "--window-size=780,860" {
			t.Fatalf("windows msedge: got (%q, %v, %v)", name, args, ok)
		}
	})

	t.Run("windows falls back to chrome then rundll32", func(t *testing.T) {
		name, args, ok := openAppWindowCommand(
			"windows", url, false, noEnv,
			func(bin string) (string, error) {
				if bin == "chrome.exe" {
					return `C:\Chrome\chrome.exe`, nil
				}
				return "", errors.New("missing")
			},
			noFile,
		)
		if !ok || name != `C:\Chrome\chrome.exe` || args[0] != "--app="+url {
			t.Fatalf("windows chrome: got (%q, %v, %v)", name, args, ok)
		}

		name, args, ok = openAppWindowCommand("windows", url, false, noEnv, noLook, noFile)
		if !ok || name != "rundll32" || len(args) != 2 || args[1] != url {
			t.Fatalf("windows fallback: got (%q, %v, %v)", name, args, ok)
		}
	})

	t.Run("darwin prefers Chrome app window and falls back to open", func(t *testing.T) {
		chrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		name, args, ok := openAppWindowCommand("darwin", url, false, noEnv, noLook, func(p string) bool { return p == chrome })
		if !ok || name != chrome || args[0] != "--app="+url {
			t.Fatalf("darwin chrome: got (%q, %v, %v)", name, args, ok)
		}
		name, args, ok = openAppWindowCommand("darwin", url, false, noEnv, noLook, noFile)
		if !ok || name != "open" || len(args) != 1 || args[0] != url {
			t.Fatalf("darwin fallback: got (%q, %v, %v)", name, args, ok)
		}
	})

	t.Run("linux respects display and prefers chromium app mode", func(t *testing.T) {
		if _, _, ok := openAppWindowCommand("linux", url, false, noEnv, noLook, noFile); ok {
			t.Fatal("linux headless should not launch a window")
		}
		name, args, ok := openAppWindowCommand("linux", url, true, noEnv, func(bin string) (string, error) {
			if bin == "chromium" {
				return "/usr/bin/chromium", nil
			}
			return "", errors.New("missing")
		}, noFile)
		if !ok || name != "/usr/bin/chromium" || args[0] != "--app="+url {
			t.Fatalf("linux chromium: got (%q, %v, %v)", name, args, ok)
		}
	})
}

func TestSingleInstanceGUIAddrReusesRunningWizard(t *testing.T) {
	dataDir := t.TempDir()
	state := &guiState{dataDir: dataDir}
	ts := httptest.NewServer(setupui.Handler(state))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	writeGUIAddr(dataDir, addr)

	gotURL, ok := probeActiveGUI(context.Background(), dataDir)
	if !ok || gotURL != ts.URL+"/setup" {
		t.Fatalf("probeActiveGUI live: got (%q, %v), want (%q, true)", gotURL, ok, ts.URL+"/setup")
	}

	// cmdGUI reuses the active wizard and invokes openSetupWindow without hanging.
	origOpen := openSetupWindow
	t.Cleanup(func() { openSetupWindow = origOpen })
	var opened string
	openSetupWindow = func(u string) error {
		opened = u
		return nil
	}
	if code := cmdGUI([]string{"--data-dir", dataDir}); code != exitOK {
		t.Fatalf("cmdGUI single-instance reuse exited %d, want 0", code)
	}
	if opened != ts.URL+"/setup" {
		t.Fatalf("cmdGUI opened %q, want %q", opened, ts.URL+"/setup")
	}

	// Stale gui.addr after server shutdown is ignored cleanly.
	ts.Close()
	if _, ok := probeActiveGUI(context.Background(), dataDir); ok {
		t.Fatal("probeActiveGUI should return false after wizard stops")
	}
	removeGUIAddrIfMatch(dataDir, addr)
	if _, err := os.Stat(filepath.Join(dataDir, guiAddrFilename)); !os.IsNotExist(err) {
		t.Fatalf("expected gui.addr removed, err=%v", err)
	}
}
