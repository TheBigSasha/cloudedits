package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
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

// TestFoldersFlagsAfterThePathAreHonored pins the documented argument order
// `folders add <dir> --json`: flags placed after the path must still apply.
func TestFoldersFlagsAfterThePathAreHonored(t *testing.T) {
	defaultDir := isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	photoDir := t.TempDir()

	code, out := runJSON(t, "folders", "add", photoDir, "--data-dir", dataDir, "--json")
	if code != exitOK || out["added"] == nil {
		t.Fatalf("folders add: code=%d out=%v", code, out)
	}
	cfg, err := config.Load(dataDir)
	if err != nil || len(cfg.Folders) != 1 {
		t.Fatalf("folder not saved under --data-dir: folders=%v err=%v", cfg.Folders, err)
	}
	if _, err := os.Stat(config.ConfigPath(defaultDir)); err == nil {
		t.Errorf("folders add wrote the default data dir although --data-dir was given")
	}

	code, out = runJSON(t, "folders", "remove", photoDir, "--data-dir", dataDir, "--json")
	if code != exitOK || out["removed"] != true {
		t.Fatalf("folders remove: code=%d out=%v", code, out)
	}
}

func writeConfigJSON(t *testing.T, dataDir, body string) {
	t.Helper()
	if err := os.WriteFile(config.ConfigPath(dataDir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestQRAndStatusUseTheEnrolledTailnetURL pins that once the node has been
// enrolled, `qr` encodes its tailnet URL (a phone cannot reach a loopback
// address) and `status` reports the URL and Tailscale IP.
func TestQRAndStatusUseTheEnrolledTailnetURL(t *testing.T) {
	isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	writeConfigJSON(t, dataDir, `{"libraryId":"lib-1","hostname":"photos","listenAddr":"127.0.0.1:8787",
		"publicUrl":"https://photos.example.ts.net","tailscaleIp":"100.64.0.7","folders":[]}`)

	if _, out := runJSON(t, "qr", "--data-dir", dataDir, "--json"); out["url"] != "https://photos.example.ts.net" {
		t.Errorf("qr url = %v, want the enrolled tailnet URL", out["url"])
	}
	if _, out := runJSON(t, "qr", "--data-dir", dataDir, "--asset", "abc", "--json"); out["url"] != "https://photos.example.ts.net/open?a=abc" {
		t.Errorf("qr asset url = %v", out["url"])
	}
	_, status := runJSON(t, "status", "--data-dir", dataDir, "--json")
	if status["url"] != "https://photos.example.ts.net" || status["tailscaleIp"] != "100.64.0.7" {
		t.Errorf("status url=%v tailscaleIp=%v, want the enrolled identity", status["url"], status["tailscaleIp"])
	}
}

// TestRecordNodeIdentityPersistsOnlyTheIdentity pins that recording the
// enrolled node's address updates the stored config without persisting
// anything else the running command changed in memory.
func TestRecordNodeIdentityPersistsOnlyTheIdentity(t *testing.T) {
	isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	writeConfigJSON(t, dataDir, `{"libraryId":"lib-1","hostname":"photos","folders":[]}`)

	if err := recordNodeIdentity(dataDir, "photos.example.ts.net", "100.64.0.7"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://photos.example.ts.net" || cfg.TailscaleIP != "100.64.0.7" || cfg.Hostname != "photos" || len(cfg.Folders) != 0 {
		t.Errorf("stored config = %+v", cfg)
	}
	// An empty identity (enrollment did not complete) must not erase a known URL.
	if err := recordNodeIdentity(dataDir, "", ""); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = config.Load(dataDir); cfg.PublicURL != "https://photos.example.ts.net" {
		t.Errorf("empty identity erased the stored URL: %q", cfg.PublicURL)
	}
}

// TestGUIStateTracksTheConfigOnDisk pins that the setup wizard reports the
// library's tailnet URL only once the node is enrolled, and picks it up when
// enrollment happens in another process while the wizard is open.
func TestGUIStateTracksTheConfigOnDisk(t *testing.T) {
	isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	writeConfigJSON(t, dataDir, `{"libraryId":"lib-1","hostname":"photos","folders":[]}`)
	state := &guiState{dataDir: dataDir}

	if got := state.PublicURL(); got != "" {
		t.Errorf("PublicURL before enrollment = %q, want empty", got)
	}
	if err := recordNodeIdentity(dataDir, "photos.example.ts.net", "100.64.0.7"); err != nil {
		t.Fatal(err)
	}
	if got := state.PublicURL(); got != "https://photos.example.ts.net" {
		t.Errorf("PublicURL after enrollment = %q", got)
	}

	// Saving a wizard edit must not erase what the other process recorded.
	cfg := state.GetConfig()
	cfg.Hostname = "renamed"
	if err := state.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if got := state.PublicURL(); got != "https://photos.example.ts.net" {
		t.Errorf("PublicURL after a wizard save = %q, want it kept", got)
	}
}

func TestRecordNodeOwnerPinsOnceAndEnvLoads(t *testing.T) {
	isolateDefaultDataDir(t)
	dataDir := t.TempDir()
	writeConfigJSON(t, dataDir, `{"libraryId":"lib-1","hostname":"photos","folders":[]}`)

	if err := recordNodeOwner(dataDir, 12345, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerUserID != 12345 || cfg.OwnerLogin != "alice@example.com" {
		t.Fatalf("got owner=(%d, %q), want (12345, alice@example.com)", cfg.OwnerUserID, cfg.OwnerLogin)
	}

	// Subsequent automatic recording must not overwrite an already-pinned ownerUserId.
	if err := recordNodeOwner(dataDir, 99999, "mallory@example.com"); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OwnerUserID != 12345 || cfg.OwnerLogin != "alice@example.com" {
		t.Fatalf("owner overwritten: got (%d, %q)", cfg.OwnerUserID, cfg.OwnerLogin)
	}

	// CLOUDEDITS_OWNER_USER_ID supplies default OwnerUserID when config.json does not set one.
	t.Setenv("CLOUDEDITS_OWNER_USER_ID", "77777")
	freshCfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if freshCfg.OwnerUserID != 77777 {
		t.Fatalf("env OwnerUserID = %d, want 77777", freshCfg.OwnerUserID)
	}
}
