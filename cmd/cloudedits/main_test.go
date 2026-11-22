package main

import (
	"path/filepath"
	"testing"
)

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
	if code := run([]string{"doctor", "--data-dir", dataDir, "--json"}); code != exitOK {
		t.Fatalf("doctor exited %d", code)
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
