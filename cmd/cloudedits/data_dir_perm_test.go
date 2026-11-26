package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A serve run must create its data directory private to the current user.
func TestSetupCreatesPrivateDataDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dataDir := filepath.Join(t.TempDir(), "state")
	if code := run([]string{"setup", "--folder", t.TempDir(), "--data-dir", dataDir, "--json"}); code != exitOK {
		t.Fatalf("setup exited %d", code)
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("data dir mode %o is accessible to other users", perm)
	}
}
