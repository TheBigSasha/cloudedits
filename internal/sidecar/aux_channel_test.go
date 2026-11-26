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

	payloadA := []byte("\x89PNG\r\n\x1a\npayload-AAAA")
	payloadB := []byte("\x89PNG\r\n\x1a\npayload-BBBB")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			payload := payloadA
			if id%2 == 1 {
				payload = payloadB
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
				if !bytes.Equal(data, payloadA) && !bytes.Equal(data, payloadB) {
					t.Errorf("observed torn aux channel bytes: %q", data)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
