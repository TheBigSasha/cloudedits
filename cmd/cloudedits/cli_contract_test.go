package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func runJSON(t *testing.T, args ...string) (int, map[string]any) {
	t.Helper()
	var code int
	out := captureStdout(t, func() { code = run(args) })
	var body map[string]any
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("%v: stdout is not JSON (%v): %q", args, err, out)
	}
	return code, body
}

// isolateDefaultDataDir points the default data directory at a temp dir so a
// command that ignores --data-dir is caught instead of touching the real one.
func isolateDefaultDataDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "default-data-dir")
	t.Setenv("CLOUDEDITS_DATA_DIR", dir)
	t.Setenv("TS_AUTHKEY", "")
	return dir
}

// TestSetupKeepsHostnameWhenFlagIsOmitted pins that re-running setup to change
// something else does not reset a previously chosen hostname.
func TestSetupKeepsHostnameWhenFlagIsOmitted(t *testing.T) {
	isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	if code, _ := runJSON(t, "setup", "--data-dir", dataDir, "--hostname", "photos", "--json"); code != exitOK {
		t.Fatalf("setup exited %d", code)
	}
	code, out := runJSON(t, "setup", "--data-dir", dataDir, "--folder", t.TempDir(), "--json")
	if code != exitOK {
		t.Fatalf("second setup exited %d", code)
	}
	if out["hostname"] != "photos" {
		t.Errorf("hostname after a setup without --hostname = %v, want %q", out["hostname"], "photos")
	}
}
