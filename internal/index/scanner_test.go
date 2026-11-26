package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
)

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
