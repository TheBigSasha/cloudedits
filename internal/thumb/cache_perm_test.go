package thumb

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Cached thumbnails are derived from the user's photos and must not be
// readable by other local accounts.
func TestThumbnailCacheIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(src, makeSampleJPEG(t, 800, 600, color.RGBA{R: 10, G: 120, B: 200, A: 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	if _, err := NewService(cacheDir, 1).Resolve(context.Background(), src, "rev-perm", "", "t1"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected cache entries, got %v (%v)", entries, err)
	}
	for _, p := range append([]string{cacheDir}, filepath.Join(cacheDir, entries[0].Name())) {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %o, accessible to other users", p, perm)
		}
	}
}
