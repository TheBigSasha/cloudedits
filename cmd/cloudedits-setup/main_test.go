package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCloudeditsBinaryPrefersSibling(t *testing.T) {
	dir := t.TempDir()
	siblingWin := filepath.Join(dir, "cloudedits.exe")
	if err := os.WriteFile(siblingWin, []byte("exe"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveCloudeditsBinary(
		filepath.Join(dir, "cloudedits-setup.exe"),
		"windows",
		os.Stat,
		func(string) (string, error) { return "/fallback/cloudedits.exe", nil },
	)
	if err != nil {
		t.Fatalf("resolveCloudeditsBinary windows sibling: %v", err)
	}
	if got != siblingWin {
		t.Fatalf("got %q, want %q", got, siblingWin)
	}

	// Falls back to LookPath when sibling does not exist.
	got, err = resolveCloudeditsBinary(
		filepath.Join(t.TempDir(), "cloudedits-setup"),
		"linux",
		os.Stat,
		func(name string) (string, error) {
			if name != "cloudedits" {
				t.Fatalf("LookPath name = %q, want cloudedits", name)
			}
			return "/usr/local/bin/cloudedits", nil
		},
	)
	if err != nil || got != "/usr/local/bin/cloudedits" {
		t.Fatalf("fallback LookPath: got (%q, %v)", got, err)
	}

	// Fails cleanly when neither sibling nor PATH has the binary.
	if _, err := resolveCloudeditsBinary(
		filepath.Join(t.TempDir(), "cloudedits-setup.exe"),
		"windows",
		os.Stat,
		func(string) (string, error) { return "", errors.New("not found") },
	); err == nil {
		t.Fatal("expected error when binary is missing")
	}
}
