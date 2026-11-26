package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thebigsasha/cloudedits/internal/config"
)

func openFDCount(t *testing.T) int {
	t.Helper()
	for _, dir := range []string{"/dev/fd", "/proc/self/fd"} {
		if entries, err := os.ReadDir(dir); err == nil {
			return len(entries)
		}
	}
	t.Skip("no per-process descriptor directory on this platform")
	return 0
}

// fastWatch shortens the watcher pacing so tests do not wait on production
// intervals.
func fastWatch(t *testing.T) {
	t.Helper()
	debounce, poll := watchDebounce, watchPollEvery
	watchDebounce, watchPollEvery = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { watchDebounce, watchPollEvery = debounce, poll })
}

func waitForCount(t *testing.T, idx *Index, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := idx.Count(context.Background(), Filter{})
		if err == nil && got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("index count = %d (err=%v), want %d", got, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWatchDoesNotHoldADescriptorPerFile pins the descriptor budget of the
// folder watcher: it must stay flat as the library grows. A watcher that
// opens every photo exhausts the process limit on a large library, after
// which the server can no longer open a socket or a source file.
func TestWatchDoesNotHoldADescriptorPerFile(t *testing.T) {
	const files = 300
	fastWatch(t)
	idx, _ := openScanned(t, files)
	root := idx.Folders()[0].Path

	before := openFDCount(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- idx.Watch(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	// A file that appears after the watcher is up proves it is live.
	writeTestJPEG(t, filepath.Join(root, "added-later.jpg"))
	waitForCount(t, idx, files+1)

	if grew := openFDCount(t) - before; grew > 32 {
		t.Fatalf("watching %d files grew the process by %d open descriptors, want a small constant", files, grew)
	}
}

// TestWatchFollowsAddRemoveAndNewSubfolder covers the change kinds a photo
// folder sees: a new file, a file in a folder created after startup, and
// removals. Both watch strategies must follow them, whichever one the host
// OS selects by default.
func TestWatchFollowsAddRemoveAndNewSubfolder(t *testing.T) {
	for name, polling := range map[string]bool{"polling": true, "notifications": false} {
		t.Run(name, func(t *testing.T) {
			byPolling := watchByPolling
			watchByPolling = polling
			t.Cleanup(func() { watchByPolling = byPolling })
			testWatchFollowsChanges(t)
		})
	}
}

func testWatchFollowsChanges(t *testing.T) {
	fastWatch(t)
	idx, _ := openScanned(t, 3)
	root := idx.Folders()[0].Path

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- idx.Watch(ctx) }()

	writeTestJPEG(t, filepath.Join(root, "new.jpg"))
	waitForCount(t, idx, 4)
	writeTestJPEG(t, filepath.Join(root, "trip", "day1", "deep.jpg"))
	waitForCount(t, idx, 5)
	if err := os.Remove(filepath.Join(root, "new.jpg")); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, idx, 4)
	if err := os.RemoveAll(filepath.Join(root, "trip")); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, idx, 3)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return after its context was cancelled")
	}
}

// TestScanKeepsIndexWhenFolderRootIsUnavailable pins that an unreadable
// folder root (an unmounted drive or share) is reported as an error and
// leaves the indexed assets in place instead of pruning the whole folder.
func TestScanKeepsIndexWhenFolderRootIsUnavailable(t *testing.T) {
	ctx := context.Background()
	idx, _ := openScanned(t, 4)
	root := idx.Folders()[0].Path
	if err := os.Rename(root, root+".unmounted"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Rename(root+".unmounted", root) }()

	if err := idx.ScanAll(ctx); err == nil {
		t.Error("ScanAll over a missing folder root returned nil, want an error")
	}
	if got, _ := idx.Count(ctx, Filter{}); got != 4 {
		t.Errorf("index count after scanning a missing root = %d, want the 4 assets kept", got)
	}
}

// TestScanContinuesPastAnUnavailableFolder pins that one unavailable folder
// does not keep the folders after it from being scanned.
func TestScanContinuesPastAnUnavailableFolder(t *testing.T) {
	ctx := context.Background()
	idx, _ := openScanned(t, 1)
	good := t.TempDir()
	writeTestJPEG(t, filepath.Join(good, "kept.jpg"))
	folders := append([]config.FolderConfig{{ID: "a-missing", Path: filepath.Join(t.TempDir(), "gone"), Label: "Missing"}}, idx.Folders()...)
	folders = append(folders, config.FolderConfig{ID: "z-good", Path: good, Label: "Good"})
	if err := idx.SetFolders(ctx, folders); err != nil {
		t.Fatal(err)
	}
	if err := idx.ScanAll(ctx); err == nil {
		t.Error("ScanAll with one missing folder returned nil, want its error")
	}
	if got, _ := idx.Count(ctx, Filter{}); got != 2 {
		t.Errorf("index count = %d, want 2 (both available folders scanned)", got)
	}
}

// TestScanExcludesColocatedAuxChannelSidecars proves that auxiliary channel
// PNG sidecars (such as colocated IMG_0001.JPG.depth.png) are never scanned or
// indexed as standalone photo assets, while real PNGs including a photo named
// depth.png remain indexed.
func TestScanExcludesColocatedAuxChannelSidecars(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()
	dataDir := t.TempDir()

	writeTestJPEG(t, filepath.Join(photoDir, "IMG_0001.JPG"))
	writeTestJPEG(t, filepath.Join(photoDir, "DCIM", "IMG_0002.JPG"))

	pngBytes := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n',
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE,
	}
	for _, rel := range []string{
		"IMG_0001.JPG.depth.png",
		"IMG_0001.ARW.DEPTH.PNG",
		filepath.Join("DCIM", "IMG_0002.JPG.depth.png"),
		filepath.Join(".hdrtoys", "IMG_0001.JPG.depth.png"),
		"depth.png",
		"normal.png",
	} {
		full := filepath.Join(photoDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, pngBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	idx, err := Open(filepath.Join(dataDir, "index.db"), "", []config.FolderConfig{
		{ID: "main", Path: photoDir, Label: "Main"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	items, total, err := idx.Range(ctx, Filter{Sort: "name", Dir: "asc"}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(items) != 4 {
		var names []string
		for _, it := range items {
			names = append(names, it.RelPath)
		}
		t.Fatalf("indexed %d assets %v, want 4 (IMG_0001.JPG, DCIM/IMG_0002.JPG, depth.png, normal.png)", total, names)
	}
	for _, it := range items {
		if it.Name == "IMG_0001.JPG.depth.png" || it.Name == "IMG_0001.ARW.DEPTH.PNG" || it.Name == "IMG_0002.JPG.depth.png" {
			t.Errorf("auxiliary sidecar %q was indexed as an asset", it.RelPath)
		}
	}
}

func BenchmarkRescanUnchanged2000(b *testing.B) {
	idx, _ := openScanned(b, 2000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := idx.ScanAll(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanCold2000(b *testing.B) {
	ctx := context.Background()
	idx, _ := openScanned(b, 2000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := idx.db.ExecContext(ctx, `DELETE FROM assets`); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := idx.ScanAll(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRangeSortByNameDeep3000(b *testing.B) {
	idx, _ := openScanned(b, 3000)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		list, _, err := idx.Range(ctx, Filter{Sort: "name", Dir: "asc"}, 2700, 200)
		if err != nil || len(list) != 200 {
			b.Fatalf("len=%d err=%v", len(list), err)
		}
	}
}
