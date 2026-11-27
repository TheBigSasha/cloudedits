package sidecar

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestValidateAuxChannel(t *testing.T) {
	suffix, err := ValidateAuxChannel("depth")
	if err != nil || suffix != ".depth.png" {
		t.Fatalf("ValidateAuxChannel(depth) = (%q, %v), want (.depth.png, nil)", suffix, err)
	}

	invalid := []string{
		"",
		"unknown",
		"DEPTH",
		"0depth",
		"-depth",
		"_depth",
		"depth.png",
		"../depth",
		"depth/sub",
		"depth\\sub",
		"depth\x00",
		strings.Repeat("a", 33),
	}
	for _, ch := range invalid {
		if _, err := ValidateAuxChannel(ch); !errors.Is(err, ErrInvalidAuxChannel) {
			t.Errorf("ValidateAuxChannel(%q) err = %v, want ErrInvalidAuxChannel", ch, err)
		}
	}
}

func TestAuxChannelWriteReadDeleteAndColocatedFallback(t *testing.T) {
	folderRoot := t.TempDir()
	sidecarRoot := filepath.Join(folderRoot, ".hdrtoys")
	rel := "DCIM/IMG_0001.JPG"

	st := NewStore()

	// 1. Absent returns os.ErrNotExist.
	if _, _, err := st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadAuxChannel when absent: err = %v, want os.ErrNotExist", err)
	}

	// 2. Colocated fallback reads <folderRoot>/<rel>.depth.png.
	colocatedData := []byte("\x89PNG\r\n\x1a\ncolocated-depth")
	colocatedPath := filepath.Join(folderRoot, "DCIM", "IMG_0001.JPG.depth.png")
	if err := os.MkdirAll(filepath.Dir(colocatedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(colocatedPath, colocatedData, 0o644); err != nil {
		t.Fatal(err)
	}
	got, src, err := st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil {
		t.Fatalf("ReadAuxChannel colocated fallback: %v", err)
	}
	if src != colocatedPath || !bytes.Equal(got, colocatedData) {
		t.Fatalf("colocated read = (%q, %q), want (%q, %q)", src, got, colocatedPath, colocatedData)
	}

	// 3. WriteAuxChannel commits atomically with file+dir fsync to <sidecarRoot>,
	// leaving colocated file untouched.
	var events []string
	restoreFile, restoreDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = restoreFile, restoreDir })
	syncFile = func(f *os.File) error {
		events = append(events, "file-sync")
		return restoreFile(f)
	}
	syncDir = func(dir string) error {
		events = append(events, "dir-sync:"+filepath.Base(dir))
		return restoreDir(dir)
	}

	canonicalData := []byte("\x89PNG\r\n\x1a\ncanonical-depth")
	writtenPath, err := st.WriteAuxChannel(sidecarRoot, rel, "depth", canonicalData)
	if err != nil {
		t.Fatalf("WriteAuxChannel: %v", err)
	}
	wantPath := filepath.Join(sidecarRoot, "DCIM", "IMG_0001.JPG.depth.png")
	if writtenPath != wantPath {
		t.Fatalf("writtenPath = %q, want %q", writtenPath, wantPath)
	}
	if !slices.Contains(events, "file-sync") {
		t.Fatalf("expected file-sync during WriteAuxChannel, got %v", events)
	}
	if events[len(events)-1] != "dir-sync:DCIM" {
		t.Fatalf("expected final dir-sync:DCIM, got %v", events)
	}

	// Canonical takes precedence over colocated on read.
	got, src, err = st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil {
		t.Fatalf("ReadAuxChannel canonical: %v", err)
	}
	if src != wantPath || !bytes.Equal(got, canonicalData) {
		t.Fatalf("canonical read = (%q, %q), want (%q, %q)", src, got, wantPath, canonicalData)
	}

	// Colocated copy remains untouched.
	afterColocated, err := os.ReadFile(colocatedPath)
	if err != nil || !bytes.Equal(afterColocated, colocatedData) {
		t.Fatalf("colocated file was modified (err=%v)", err)
	}

	// 4. DeleteAuxChannel removes only canonical file under sidecarRoot.
	deleted, err := st.DeleteAuxChannel(sidecarRoot, rel, "depth")
	if err != nil || !deleted {
		t.Fatalf("DeleteAuxChannel first = (%v, %v), want (true, nil)", deleted, err)
	}
	deletedAgain, err := st.DeleteAuxChannel(sidecarRoot, rel, "depth")
	if err != nil || deletedAgain {
		t.Fatalf("DeleteAuxChannel second = (%v, %v), want (false, nil)", deletedAgain, err)
	}

	// Colocated file is still untouched and becomes the read fallback again.
	got, src, err = st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil || src != colocatedPath || !bytes.Equal(got, colocatedData) {
		t.Fatalf("read after delete = (%q, %q, %v), want colocated fallback", src, got, err)
	}

	// Invalid channel errors on all methods.
	if _, _, err := st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "../bad"); !errors.Is(err, ErrInvalidAuxChannel) {
		t.Fatalf("ReadAuxChannel invalid channel err = %v", err)
	}
	if _, err := st.WriteAuxChannel(sidecarRoot, rel, "bad.chan", canonicalData); !errors.Is(err, ErrInvalidAuxChannel) {
		t.Fatalf("WriteAuxChannel invalid channel err = %v", err)
	}
	if _, err := st.DeleteAuxChannel(sidecarRoot, rel, "unknown"); !errors.Is(err, ErrInvalidAuxChannel) {
		t.Fatalf("DeleteAuxChannel invalid channel err = %v", err)
	}
}

func TestAuxChannelConcurrentAccessIsSerialized(t *testing.T) {
	folderRoot := t.TempDir()
	sidecarRoot := filepath.Join(folderRoot, ".hdrtoys")
	st := NewStore()

	payloadPNG := []byte("\x89PNG\r\n\x1a\npayload-PNG-AAAA")
	payloadJPG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'J', 'P', 'E', 'G', '-', 'B', 'B', 'B', 'B', 0xFF, 0xD9}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := payloadPNG
			if id%2 == 1 {
				payload = payloadJPG
			}
			for j := 0; j < 10; j++ {
				if _, err := st.WriteAuxChannel(sidecarRoot, "IMG_0001.JPG", "depth", payload); err != nil {
					t.Errorf("WriteAuxChannel: %v", err)
					return
				}
				data, _, err := st.ReadAuxChannel(folderRoot, sidecarRoot, "IMG_0001.JPG", "depth")
				if err != nil {
					t.Errorf("ReadAuxChannel: %v", err)
					return
				}
				if !bytes.Equal(data, payloadPNG) && !bytes.Equal(data, payloadJPG) {
					t.Errorf("observed torn aux channel bytes: %q", data)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// Single asset-channel lock guarantees only one format sibling remains on disk.
	_, errJPG := os.Stat(filepath.Join(sidecarRoot, "IMG_0001.JPG.depth.jpg"))
	_, errPNG := os.Stat(filepath.Join(sidecarRoot, "IMG_0001.JPG.depth.png"))
	if (errJPG == nil) == (errPNG == nil) {
		t.Fatalf("expected exactly one format sibling after concurrent writes (jpg err=%v, png err=%v)", errJPG, errPNG)
	}
}

func TestAuxChannelDualFormatSwitchNewestMtimeAndDelete(t *testing.T) {
	folderRoot := t.TempDir()
	sidecarRoot := filepath.Join(folderRoot, ".hdrtoys")
	rel := "DCIM/IMG_0002.JPG"
	st := NewStore()

	jpegV1 := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'd', 'e', 'p', 't', 'h', '1', 0xFF, 0xD9}
	pngV1 := []byte("\x89PNG\r\n\x1a\ndepth-png-v1")
	jpegV2 := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'd', 'e', 'p', 't', 'h', '2', 0xFF, 0xD9}

	// Unsupported magic fails with ErrUnsupportedAuxFormat.
	if _, err := st.WriteAuxChannel(sidecarRoot, rel, "depth", []byte("not-an-image")); !errors.Is(err, ErrUnsupportedAuxFormat) {
		t.Fatalf("WriteAuxChannel invalid payload err = %v, want ErrUnsupportedAuxFormat", err)
	}

	// 1. Write JPEG -> creates .depth.jpg.
	jpgPath, err := st.WriteAuxChannel(sidecarRoot, rel, "depth", jpegV1)
	if err != nil {
		t.Fatalf("WriteAuxChannel(JPEG): %v", err)
	}
	wantJPG := filepath.Join(sidecarRoot, "DCIM", "IMG_0002.JPG.depth.jpg")
	wantPNG := filepath.Join(sidecarRoot, "DCIM", "IMG_0002.JPG.depth.png")
	if jpgPath != wantJPG {
		t.Fatalf("jpgPath = %q, want %q", jpgPath, wantJPG)
	}
	got, src, err := st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil || src != wantJPG || !bytes.Equal(got, jpegV1) {
		t.Fatalf("ReadAuxChannel(JPEG) = (%q, %d bytes, %v)", src, len(got), err)
	}

	// 2. Switch to PNG -> writes .depth.png and removes stale .depth.jpg.
	pngPath, err := st.WriteAuxChannel(sidecarRoot, rel, "depth", pngV1)
	if err != nil || pngPath != wantPNG {
		t.Fatalf("WriteAuxChannel(PNG) = (%q, %v), want (%q, nil)", pngPath, err, wantPNG)
	}
	if _, err := os.Stat(wantJPG); !os.IsNotExist(err) {
		t.Fatalf("stale .depth.jpg was not removed when switching to PNG: err=%v", err)
	}

	// 3. Switch back to JPEG -> writes .depth.jpg and removes stale .depth.png.
	if _, err := st.WriteAuxChannel(sidecarRoot, rel, "depth", jpegV2); err != nil {
		t.Fatalf("WriteAuxChannel(JPEG v2): %v", err)
	}
	if _, err := os.Stat(wantPNG); !os.IsNotExist(err) {
		t.Fatalf("stale .depth.png was not removed when switching to JPEG: err=%v", err)
	}

	// 4. When both .depth.jpg and .depth.png coexist on disk, ReadAuxChannel prefers newest mtime.
	if err := os.WriteFile(wantPNG, pngV1, 0o644); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-2 * time.Minute)
	newer := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(wantJPG, older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(wantPNG, newer, newer); err != nil {
		t.Fatal(err)
	}
	got, src, err = st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil || src != wantPNG || !bytes.Equal(got, pngV1) {
		t.Fatalf("newest-mtime (PNG newer): got (%q, %v), want %q", src, err, wantPNG)
	}
	// Flip mtimes so JPEG is newer.
	evenNewer := time.Now()
	if err := os.Chtimes(wantJPG, evenNewer, evenNewer); err != nil {
		t.Fatal(err)
	}
	got, src, err = st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil || src != wantJPG || !bytes.Equal(got, jpegV2) {
		t.Fatalf("newest-mtime (JPEG newer): got (%q, %v), want %q", src, err, wantJPG)
	}

	// 5. DeleteAuxChannel removes BOTH .depth.jpg and .depth.png under sidecarRoot.
	deleted, err := st.DeleteAuxChannel(sidecarRoot, rel, "depth")
	if err != nil || !deleted {
		t.Fatalf("DeleteAuxChannel both = (%v, %v), want (true, nil)", deleted, err)
	}
	if _, err := os.Stat(wantJPG); !os.IsNotExist(err) {
		t.Fatalf("expected .depth.jpg deleted, err=%v", err)
	}
	if _, err := os.Stat(wantPNG); !os.IsNotExist(err) {
		t.Fatalf("expected .depth.png deleted, err=%v", err)
	}

	// 6. Mismatched magic on disk (e.g. PNG bytes in .depth.jpg) is rejected as absent.
	if err := os.WriteFile(wantJPG, pngV1, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched magic read err = %v, want os.ErrNotExist", err)
	}
	_, _ = st.DeleteAuxChannel(sidecarRoot, rel, "depth")

	// 7. Colocated fallback supports both .depth.jpg and .depth.png with newest-mtime preference.
	colocJPG := filepath.Join(folderRoot, "DCIM", "IMG_0002.JPG.depth.jpg")
	colocPNG := filepath.Join(folderRoot, "DCIM", "IMG_0002.JPG.depth.png")
	if err := os.MkdirAll(filepath.Dir(colocJPG), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(colocJPG, jpegV1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(colocPNG, pngV1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(colocPNG, older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(colocJPG, newer, newer); err != nil {
		t.Fatal(err)
	}
	got, src, err = st.ReadAuxChannel(folderRoot, sidecarRoot, rel, "depth")
	if err != nil || src != colocJPG || !bytes.Equal(got, jpegV1) {
		t.Fatalf("colocated newest-mtime JPEG: got (%q, %v), want %q", src, err, colocJPG)
	}
}
