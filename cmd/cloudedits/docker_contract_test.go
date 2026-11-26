package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
)

func dockerfileCmd(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^CMD (\[.*\])\s*$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("Dockerfile has no exec-form CMD")
	}
	var cmd []string
	if err := json.Unmarshal(m[1], &cmd); err != nil {
		t.Fatalf("Dockerfile CMD is not a JSON array: %v", err)
	}
	return cmd
}

// TestImageDefaultCommandServesTheMountedLibrary pins that the image works
// with the compose file the usage guide documents, which sets no command: the
// default must serve the documented /library/photos mount over the tailnet,
// not bind an unreachable loopback port inside the container with no folder.
func TestImageDefaultCommandServesTheMountedLibrary(t *testing.T) {
	cmd := strings.Join(dockerfileCmd(t), " ")
	for _, want := range []string{"serve", "--tsnet", "--folder /library/photos"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("Dockerfile CMD %q lacks %q", cmd, want)
		}
	}
}

// TestGeneratedComposeMatchesImageDefaultCommand keeps `docker-init` and the
// image from drifting apart.
func TestGeneratedComposeMatchesImageDefaultCommand(t *testing.T) {
	composePath, _, err := config.WriteDockerBundle(t.TempDir(), "/volume1/photos", "library")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(dockerfileCmd(t))
	normalized := strings.ReplaceAll(string(compose), `", "`, `","`)
	if !strings.Contains(normalized, "command: "+string(want)) {
		t.Errorf("generated compose command differs from the image default %s:\n%s", want, compose)
	}
}

// TestDockerSidecarPermissionsContract pins that the Docker image and compose
// bundle support matching host UID/GID (PUID/PGID) so colocated .hdrtoys/
// sidecar writes succeed on NAS bind mounts without losing access to /data.
func TestDockerSidecarPermissionsContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	df := string(raw)
	for _, want := range []string{"USER cloudedits", "/data/sidecars", "chmod 1777 /data /data/sidecars"} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}

	composePath, envPath, err := config.WriteDockerBundle(t.TempDir(), "/volume1/photos", "library")
	if err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	envFile, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), `user: "${PUID:-10001}:${PGID:-10001}"`) {
		t.Errorf("compose missing PUID/PGID user mapping:\n%s", compose)
	}
	for _, want := range []string{"PUID=10001", "PGID=10001", "CLOUDEDITS_SIDECAR_DIR", "CLOUDEDITS_OWNER_USER_ID"} {
		if !strings.Contains(string(envFile), want) {
			t.Errorf(".env.example missing %q:\n%s", want, envFile)
		}
	}
}

func TestWarnUnwritableSidecarsDoesNotMutateReadOnlyFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX non-root permission test")
	}
	photoDir := t.TempDir()
	if err := os.Chmod(photoDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(photoDir, 0o755) })

	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	cfg := &config.Config{
		Folders: []config.FolderConfig{{ID: "photos", Path: photoDir, Label: "Photos"}},
	}
	warnUnwritableSidecars(cfg, logf)
	if len(logs) != 1 || !strings.Contains(logs[0], "not writable") {
		t.Fatalf("expected 1 unwritable sidecar warning, got %v", logs)
	}
	if _, err := os.Stat(filepath.Join(photoDir, ".hdrtoys")); !os.IsNotExist(err) {
		t.Fatalf("startup check must not create .hdrtoys inside photo folder: err=%v", err)
	}

	// Pointing SidecarDir at a writable directory clears the warning.
	logs = nil
	cfg.SidecarDir = t.TempDir()
	warnUnwritableSidecars(cfg, logf)
	if len(logs) != 0 {
		t.Fatalf("expected no warning with writable SidecarDir, got %v", logs)
	}
}
