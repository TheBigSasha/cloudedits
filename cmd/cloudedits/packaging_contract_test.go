package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAppImageBuildPinsVersionsAndChecksums ensures the Linux AppImage packager
// never fetches unverified rolling "continuous" releases of appimagetool or
// type2-runtime.
func TestAppImageBuildPinsVersionsAndChecksums(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "packaging", "linux", "build-appimage.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	if strings.Contains(script, "/download/continuous/") {
		t.Fatal("build-appimage.sh still fetches unverified rolling /download/continuous/ releases")
	}
	for _, want := range []string{
		`APPIMAGETOOL_VERSION="`,
		`TYPE2_RUNTIME_VERSION="`,
		`verify_sha256 "$APPIMAGETOOL"`,
		`verify_sha256 "$runtime_file"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("build-appimage.sh missing %q", want)
		}
	}
	shaVars := []string{
		"APPIMAGETOOL_SHA256_x86_64",
		"APPIMAGETOOL_SHA256_aarch64",
		"TYPE2_RUNTIME_SHA256_x86_64",
		"TYPE2_RUNTIME_SHA256_aarch64",
	}
	for _, name := range shaVars {
		re := regexp.MustCompile(`(?m)^` + name + `="[0-9a-f]{64}"$`)
		if !re.MatchString(script) {
			t.Errorf("build-appimage.sh missing pinned 64-hex SHA-256 variable %s", name)
		}
	}
}
