//go:build !windows

package sidecar

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSidecarWritesConfinedByOSRoot verifies O7: symlinked directories inside
// .hdrtoys/ (or .hdrtoys itself, or .tmp) can never redirect writes, renames,
// deletes, or reads outside sidecarRoot.
func TestSidecarWritesConfinedByOSRoot(t *testing.T) {
	store := NewStore()
	validPNG := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n', 0, 0, 0, 0}
	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0xFF, 0xD9}

	t.Run("symlinked subdir escapes sidecar root on write and delete", func(t *testing.T) {
		outside := t.TempDir()
		victim := filepath.Join(outside, "IMG.JPG.depth.png")
		if err := os.WriteFile(victim, validPNG, 0o644); err != nil {
			t.Fatal(err)
		}

		folder := t.TempDir()
		root := filepath.Join(folder, ".hdrtoys")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
			t.Fatal(err)
		}

		// WriteAtomic (.edits) must fail and must not write into outside.
		target, err := CanonicalEditsPath(root, "sub/IMG.JPG")
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteAtomic(root, target, []byte(tinyTruth)); err == nil {
			t.Fatal("WriteAtomic escaped sidecar root through symlinked subdir")
		}
		if _, err := os.Stat(filepath.Join(outside, "IMG.JPG.edits")); !os.IsNotExist(err) {
			t.Fatalf("outside .edits created via symlinked subdir: err=%v", err)
		}

		// Store.Update must fail and must not write into outside.
		if _, _, err := store.Update(folder, root, "sub/IMG.JPG", nil, "", func(sc *Sidecar) (bool, error) {
			sc.Fields["rating"] = FieldCell{HLC: "019a2f0012ab-0001-devA", V: 5}
			return true, nil
		}); err == nil {
			t.Fatal("Store.Update escaped sidecar root through symlinked subdir")
		}

		// WriteEditThumbnail must fail.
		if _, err := store.WriteEditThumbnail(root, "sub/IMG.JPG", validJPEG); err == nil {
			t.Fatal("WriteEditThumbnail escaped sidecar root through symlinked subdir")
		}
		if _, err := os.Stat(filepath.Join(outside, "IMG.JPG.edit_thumbnail.jpg")); !os.IsNotExist(err) {
			t.Fatalf("outside edit thumbnail created via symlinked subdir: err=%v", err)
		}

		// WriteAuxChannel must fail and leave outside victim untouched.
		if _, err := store.WriteAuxChannel(root, "sub/IMG.JPG", "depth", validPNG); err == nil {
			t.Fatal("WriteAuxChannel escaped sidecar root through symlinked subdir")
		}

		// DeleteAuxChannel must fail and must NOT delete victim outside sidecarRoot.
		if deleted, err := store.DeleteAuxChannel(root, "sub/IMG.JPG", "depth"); err == nil || deleted {
			t.Fatalf("DeleteAuxChannel escaped sidecar root: deleted=%v err=%v", deleted, err)
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("victim outside sidecar root was deleted: %v", err)
		}

		// ReadAuxChannel must not disclose victim through symlinked subdir.
		if _, _, err := store.ReadAuxChannel(folder, root, "sub/IMG.JPG", "depth"); err == nil {
			t.Fatal("ReadAuxChannel escaped sidecar root through symlinked subdir")
		}

		// SaveRender must fail when renders subdir is symlinked outside.
		if err := os.MkdirAll(filepath.Join(root, "renders"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "renders", "escape")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.SaveRender(root, "escape", "r1", "", "image/jpeg", validJPEG); err == nil {
			t.Fatal("SaveRender escaped sidecar root through symlinked renders subdir")
		}
		if _, err := os.Stat(filepath.Join(outside, "r1.jpg")); !os.IsNotExist(err) {
			t.Fatalf("outside render created via symlinked subdir: err=%v", err)
		}
	})

	t.Run("relative traversal symlink inside sidecar root is rejected", func(t *testing.T) {
		parent := t.TempDir()
		outside := filepath.Join(parent, "outside")
		folder := filepath.Join(parent, "photos")
		root := filepath.Join(folder, ".hdrtoys")
		if err := os.MkdirAll(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../outside", filepath.Join(root, "rel")); err != nil {
			t.Fatal(err)
		}
		target, err := CanonicalEditsPath(root, "rel/IMG.JPG")
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteAtomic(root, target, []byte(tinyTruth)); err == nil {
			t.Fatal("WriteAtomic followed relative traversal symlink outside sidecar root")
		}
		if _, err := os.Stat(filepath.Join(outside, "IMG.JPG.edits")); !os.IsNotExist(err) {
			t.Fatalf("outside file written via relative symlink: err=%v", err)
		}
	})

	t.Run("symlinked .tmp directory is rejected", func(t *testing.T) {
		outside := t.TempDir()
		root := filepath.Join(t.TempDir(), ".hdrtoys")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, ".tmp")); err != nil {
			t.Fatal(err)
		}
		target, err := CanonicalEditsPath(root, "IMG.JPG")
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteAtomic(root, target, []byte(tinyTruth)); err == nil {
			t.Fatal("WriteAtomic staged through symlinked .tmp outside sidecar root")
		}
		entries, err := os.ReadDir(outside)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("files created in outside dir via symlinked .tmp: %v", entries)
		}
	})

	t.Run("symlinked sidecar root itself is rejected", func(t *testing.T) {
		outside := t.TempDir()
		folder := t.TempDir()
		root := filepath.Join(folder, ".hdrtoys")
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
		target, err := CanonicalEditsPath(root, "IMG.JPG")
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteAtomic(root, target, []byte(tinyTruth)); err == nil {
			t.Fatal("WriteAtomic wrote through symlinked sidecar root")
		}
	})

	t.Run("EditThumbnailStamp ignores symlinks to match EditThumbnailStamps", func(t *testing.T) {
		outside := t.TempDir()
		realThumb := filepath.Join(outside, "thumb.jpg")
		if err := os.WriteFile(realThumb, validJPEG, 0o644); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(t.TempDir(), ".hdrtoys")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realThumb, filepath.Join(root, "IMG.JPG.edit_thumbnail.jpg")); err != nil {
			t.Fatal(err)
		}
		if stamp := EditThumbnailStamp(root, "IMG.JPG"); stamp != 0 {
			t.Fatalf("EditThumbnailStamp followed symlink and returned %d, want 0", stamp)
		}
		if stamps := EditThumbnailStamps(root); len(stamps) != 0 {
			t.Fatalf("EditThumbnailStamps returned %v, want empty", stamps)
		}
	})
}
