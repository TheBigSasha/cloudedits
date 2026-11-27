//go:build !windows

package thumb

import (
	"bytes"
	"context"
	"image/color"
	"os"
	"path/filepath"
	"testing"
)

// The edited-preview thumbnail is read from the sidecar folder, which other
// software can write to. A symbolic link there must not be served and an
// oversized file must not be read into memory; the generated thumbnail is
// served instead.
func TestEditThumbnailTrustsOnlyBoundedRegularFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(src, makeSampleJPEG(t, 800, 600, color.RGBA{R: 10, G: 120, B: 200, A: 255}), 0o644); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(t.TempDir(), "private.jpg")
	privateBytes := makeSampleJPEG(t, 64, 64, color.RGBA{R: 255, A: 255})
	if err := os.WriteFile(private, privateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	editThumb := filepath.Join(dir, "a.jpg.edit_thumbnail.jpg")
	svc := NewService(filepath.Join(dir, "cache"), 1)

	if err := os.Symlink(private, editThumb); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(context.Background(), src, "rev-link", editThumb, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if res.TierSource == "edit-thumbnail" || bytes.Equal(res.Data, privateBytes) {
		t.Fatalf("served a file outside the library through a symbolic link (source %q)", res.TierSource)
	}

	_ = os.Remove(editThumb)
	f, err := os.Create(editThumb)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(privateBytes)
	if err := f.Truncate(maxEditThumbnailReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	res, err = svc.Resolve(context.Background(), src, "rev-big", editThumb, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if res.TierSource == "edit-thumbnail" || len(res.Data) > 1<<20 {
		t.Fatalf("served an oversized edit thumbnail (%d bytes, source %q)", len(res.Data), res.TierSource)
	}

	// A genuine edit thumbnail still takes priority.
	_ = os.Remove(editThumb)
	if err := os.WriteFile(editThumb, privateBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = svc.Resolve(context.Background(), src, "rev-ok", editThumb, "t1")
	if err != nil || res.TierSource != "edit-thumbnail" || !bytes.Equal(res.Data, privateBytes) {
		t.Fatalf("genuine edit thumbnail not served: %+v, %v", res, err)
	}
}
