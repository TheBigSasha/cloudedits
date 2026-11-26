//go:build !windows

package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tinyTruth = `{"hdrt":1,"asset":{"file":"IMG.JPG"},"document":{"role":"truth"},"fields":{"rating":{"hlc":"019a2f0012ab-0001-devA","v":5}}}`

// Sidecars are read from the sidecar folder and from beside the photo, both
// writable by other software. A symbolic link there must not be followed and
// an oversized file must not be read: the library scan reads every asset's
// sidecar at startup, so one such entry would otherwise stall or exhaust the
// server before it serves anything.
func TestSidecarReadTrustsOnlyBoundedRegularFiles(t *testing.T) {
	outside := t.TempDir()
	foreign := filepath.Join(outside, "foreign.edits")
	if err := os.WriteFile(foreign, []byte(tinyTruth), 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(outside, "oversized.edits")
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(tinyTruth)
	if err := f.Truncate(MaxSidecarBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	newLibrary := func(t *testing.T) (folder, root string) {
		t.Helper()
		folder = t.TempDir()
		root = filepath.Join(folder, ".hdrtoys")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		return folder, root
	}
	store := NewStore()

	t.Run("link in sidecar folder", func(t *testing.T) {
		folder, root := newLibrary(t)
		if err := os.Symlink(foreign, filepath.Join(root, "IMG.JPG.edits")); err != nil {
			t.Fatal(err)
		}
		res, err := store.Read(folder, root, "IMG.JPG")
		if err == nil {
			t.Fatalf("followed a symbolic link to %s: %+v", foreign, res)
		}
		if _, statErr := os.Lstat(filepath.Join(root, ".quarantine")); statErr == nil {
			t.Fatal("a symbolic link was quarantined as if it were a corrupt sidecar")
		}
	})

	t.Run("oversized file in sidecar folder", func(t *testing.T) {
		folder, root := newLibrary(t)
		target := filepath.Join(root, "IMG.JPG.edits")
		if err := os.Link(oversized, target); err != nil {
			t.Skipf("hard link unavailable: %v", err)
		}
		if _, err := store.Read(folder, root, "IMG.JPG"); err == nil {
			t.Fatal("read a sidecar over the size limit")
		}
		if _, statErr := os.Lstat(target); statErr != nil {
			t.Fatalf("the oversized sidecar was moved away instead of left for the user: %v", statErr)
		}
	})

	t.Run("link and oversized file beside photo are ignored", func(t *testing.T) {
		folder, root := newLibrary(t)
		if err := os.Symlink(foreign, filepath.Join(folder, "IMG.JPG.edits")); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(oversized, filepath.Join(folder, "IMG.JPG.hdrt.json")); err != nil {
			t.Skipf("hard link unavailable: %v", err)
		}
		res, err := store.Read(folder, root, "IMG.JPG")
		if err != nil || res.Sidecar != nil {
			t.Fatalf("expected no sidecar and no error, got %+v, %v", res, err)
		}
	})

	t.Run("a write that would exceed the limit is refused", func(t *testing.T) {
		folder, root := newLibrary(t)
		target := filepath.Join(root, "IMG.JPG.edits")
		if err := os.WriteFile(target, []byte(tinyTruth), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.Update(folder, root, "IMG.JPG", nil, "", func(sc *Sidecar) (bool, error) {
			sc.Fields["note"] = FieldCell{HLC: "019a2f0012ab-0002-devA", V: strings.Repeat("A", MaxSidecarBytes)}
			return true, nil
		})
		if err == nil {
			t.Fatal("wrote a sidecar larger than the server will read back")
		}
		if data, _ := os.ReadFile(target); string(data) != tinyTruth {
			t.Fatalf("the existing sidecar changed (%d bytes)", len(data))
		}
		if res, err := store.Read(folder, root, "IMG.JPG"); err != nil || res.Sidecar == nil {
			t.Fatalf("existing sidecar no longer readable: %+v, %v", res, err)
		}
	})
}
