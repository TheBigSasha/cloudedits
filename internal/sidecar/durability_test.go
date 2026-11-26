package sidecar

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// An acknowledged edit must survive power loss: the staged bytes are flushed
// before the rename makes them visible, and the directory entry the rename
// created is flushed afterwards. Without the first flush a crash can surface a
// zero-length or torn sidecar under the final name; without the second the
// rename itself can be lost after the client was told the write is durable.
func TestWriteAtomicFlushesFileBeforeRenameAndDirectoryAfter(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".hdrtoys")
	target := filepath.Join(root, "DCIM", "A.JPG.edits")
	if err := WriteAtomic(root, target, []byte("old\n")); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	var events []string
	onDisk := func() string {
		b, _ := os.ReadFile(target)
		return string(b)
	}
	restoreFile, restoreDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = restoreFile, restoreDir })
	syncFile = func(f *os.File) error {
		events = append(events, "file-flush while target="+onDisk())
		return restoreFile(f)
	}
	syncDir = func(dir string) error {
		events = append(events, "dir-flush "+filepath.Base(dir)+" while target="+onDisk())
		return restoreDir(dir)
	}

	if err := WriteAtomic(root, target, []byte("new\n")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	want := []string{"file-flush while target=old\n", "dir-flush DCIM while target=new\n"}
	if !slices.Equal(events, want) {
		t.Fatalf("flush sequence = %q, want %q", events, want)
	}
}

// A process killed between creating a staging file and renaming it leaves the
// file behind. Nothing ever removed those, so they accumulated forever.
func TestWriteAtomicSweepsStaleStagingFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".hdrtoys")
	tmpDir := filepath.Join(root, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmpDir, "0123456789abcdef0123456789abcdef.tmp")
	fresh := filepath.Join(tmpDir, "fedcba9876543210fedcba9876543210.tmp")
	foreign := filepath.Join(tmpDir, "notes.txt")
	for _, p := range []string{stale, fresh, foreign} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleStagingAge)
	for _, p := range []string{stale, foreign} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if err := WriteAtomic(root, filepath.Join(root, "A.JPG.edits"), []byte("{}\n")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staging file survived (err=%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a staging file another writer may still be using was removed: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a file this server did not create was removed: %v", err)
	}
}

// Edited-preview thumbnails are disposable and rebuildable, so they are staged
// and renamed atomically but never pay for a flush.
func TestWriteEditThumbnailDoesNotFlush(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".hdrtoys")
	flushes := 0
	restoreFile, restoreDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = restoreFile, restoreDir })
	syncFile = func(*os.File) error { flushes++; return nil }
	syncDir = func(string) error { flushes++; return nil }
	if _, err := NewStore().WriteEditThumbnail(root, "A.JPG", []byte{0xFF, 0xD8, 0xFF, 0xD9}); err != nil {
		t.Fatalf("WriteEditThumbnail: %v", err)
	}
	if flushes != 0 {
		t.Fatalf("edit thumbnail write flushed %d times", flushes)
	}
}
