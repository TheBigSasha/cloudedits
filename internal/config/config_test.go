package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSaveLoadFoldersAndDockerBundle(t *testing.T) {
	dataDir := t.TempDir()
	photos := t.TempDir()

	cfg, err := Load(dataDir)
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	fc, err := cfg.AddFolder(photos, "Vacation")
	if err != nil {
		t.Fatalf("AddFolder: %v", err)
	}
	if fc.ID != "vacation" {
		t.Fatalf("unexpected folder id: %q", fc.ID)
	}
	// Duplicate path is idempotent.
	if _, err := cfg.AddFolder(photos, "Vacation"); err != nil {
		t.Fatalf("duplicate AddFolder: %v", err)
	}
	if len(cfg.Folders) != 1 {
		t.Fatalf("expected 1 deduplicated folder, got %d", len(cfg.Folders))
	}

	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(dataDir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(loaded.Folders) != 1 || loaded.Folders[0].Path != fc.Path {
		t.Fatalf("reloaded folders mismatch: %+v", loaded.Folders)
	}
	if !loaded.RemoveFolder(fc.ID) || len(loaded.Folders) != 0 {
		t.Fatalf("expected RemoveFolder to clear folder")
	}

	outDir := filepath.Join(t.TempDir(), "docker")
	composePath, envPath, err := WriteDockerBundle(outDir, "/volume1/photos", "mylib")
	if err != nil {
		t.Fatalf("WriteDockerBundle: %v", err)
	}
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), "CLOUDEDITS_HOSTNAME: mylib") || strings.Contains(string(compose), "NET_ADMIN") {
		t.Fatalf("unexpected docker-compose.yml:\n%s", string(compose))
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Fatalf("expected .env.example: %v", err)
	}
}
