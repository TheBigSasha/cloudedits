package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
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
