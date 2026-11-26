package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

// TestSetupReportsThatNoServiceWasInstalled pins that setup output never
// claims a background service: nothing is serving until `serve` runs, and the
// output must say how to start it.
func TestSetupReportsThatNoServiceWasInstalled(t *testing.T) {
	isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	code, out := runJSON(t, "setup", "--data-dir", dataDir, "--install-service", "--json")
	if code != exitOK {
		t.Fatalf("setup exited %d", code)
	}
	if v, ok := out["serviceInstalled"].(bool); !ok || v {
		t.Errorf("serviceInstalled = %v, want false", out["serviceInstalled"])
	}
	if _, claims := out["installService"]; claims {
		t.Errorf("output still carries installService=%v although no service is installed", out["installService"])
	}
	cmd, _ := out["serveCommand"].([]any)
	if len(cmd) < 3 || cmd[0] != "cloudedits" || cmd[1] != "serve" {
		t.Fatalf("serveCommand = %v, want the command that starts the server", out["serveCommand"])
	}
	joined := ""
	for _, part := range cmd {
		joined += part.(string) + " "
	}
	if !strings.Contains(joined, "--tsnet") || !strings.Contains(joined, dataDir) {
		t.Errorf("serveCommand %q must serve over the tailnet from the configured data dir", joined)
	}
}
