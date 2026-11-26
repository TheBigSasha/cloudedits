package thumb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A scan window that ends in a bare JPEG SOI (FF D8 FF) must not index past
// the buffer. The embedded-preview scanner reads fixed 2/8/12 MiB windows, so
// any container with an SOI on a window boundary (or at EOF) hit this.
func TestEmbeddedScanToleratesSOIAtWindowEnd(t *testing.T) {
	for _, data := range [][]byte{
		{0x00, 0x00, 0xFF, 0xD8, 0xFF},
		append(bytes.Repeat([]byte{0x11}, 4096), 0xFF, 0xD8, 0xFF),
		{0xFF, 0xD8, 0xFF},
	} {
		if best, _ := findBestEmbeddedJPEGForTarget(data, 480); best != nil {
			t.Fatalf("expected no candidate, got %d bytes", len(best))
		}
	}

	dir := t.TempDir()
	raw := filepath.Join(dir, "IMG_0001.DNG")
	if err := os.WriteFile(raw, append([]byte{'I', 'I', 0x2A, 0x00, 0x08, 0, 0, 0}, 0xFF, 0xD8, 0xFF), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := NewService(filepath.Join(dir, "cache"), 1).Resolve(context.Background(), raw, "rev-soi-eof", "", "t1")
	if err == nil || strings.Contains(err.Error(), "panic") {
		t.Fatalf("expected a clean extraction error, got %v", err)
	}
}
