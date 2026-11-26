package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
