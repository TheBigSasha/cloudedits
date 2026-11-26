package index

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

func openScanned(tb testing.TB, files int) (*Index, []string) {
	tb.Helper()
	photoDir := tb.TempDir()
	ids := make([]string, files)
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("IMG_%04d.jpg", i)
		writeTestJPEGFile(tb, filepath.Join(photoDir, name))
		ids[i] = MakeAssetID("p", name)
	}
	folders := []config.FolderConfig{{ID: "p", Path: photoDir, Label: "P"}}
	idx, err := Open(filepath.Join(tb.TempDir(), "index.db"), "", folders, sidecar.NewStore())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = idx.Close() })
	if err := idx.ScanAll(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return idx, ids
}

// TestChangesCursorAheadOfJournalRequiresSnapshot pins the recovery path for a
// client whose cursor outlived the journal (the rebuildable index was deleted
// and re-created): the feed must say so instead of echoing the dead cursor
// back forever.
func TestChangesCursorAheadOfJournalRequiresSnapshot(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 2)
	ops := []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-d", Device: "d", AssetKey: ids[0], Field: "rating", Value: 3},
		{HLC: "019a2f001000-0002-d", Device: "d", AssetKey: ids[1], Field: "rating", Value: 4},
	}
	if _, err := idx.ApplyOpsBatch(ctx, ops); err != nil {
		t.Fatal(err)
	}

	head, err := idx.ChangesSince(ctx, "", 0)
	if err != nil || len(head.Ops) != 2 || head.SnapshotRequired {
		t.Fatalf("bootstrap: ops=%d snapshotRequired=%v err=%v", len(head.Ops), head.SnapshotRequired, err)
	}
	caughtUp, err := idx.ChangesSince(ctx, head.NextCursor, 0)
	if err != nil || len(caughtUp.Ops) != 0 || caughtUp.SnapshotRequired || caughtUp.NextCursor != head.NextCursor {
		t.Fatalf("caught-up poll must be a quiet no-op, got %+v err=%v", caughtUp, err)
	}

	ahead, err := idx.ChangesSince(ctx, "c999", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ahead.SnapshotRequired {
		t.Errorf("cursor ahead of the journal: snapshotRequired = false, want true")
	}
	if ahead.NextCursor != head.NextCursor {
		t.Errorf("cursor ahead of the journal: nextCursor = %q, want journal head %q", ahead.NextCursor, head.NextCursor)
	}
}

// TestChangesFeedAssetsAreTouchedSetOrderedByID guards the shape of the
// assets[] half of a feed page: exactly the assets the page's ops touched,
// each once, ordered by ID.
func TestChangesFeedAssetsAreTouchedSetOrderedByID(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 5)
	ops := []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-d", Device: "d", AssetKey: ids[3], Field: "rating", Value: 3},
		{HLC: "019a2f001000-0002-d", Device: "d", AssetKey: ids[1], Field: "rating", Value: 4},
		{HLC: "019a2f001000-0003-d", Device: "d", AssetKey: ids[3], Field: "flag", Value: "pick"},
	}
	if _, err := idx.ApplyOpsBatch(ctx, ops); err != nil {
		t.Fatal(err)
	}
	feed, err := idx.ChangesSince(ctx, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Assets) != 2 || feed.Assets[0].ID >= feed.Assets[1].ID {
		t.Fatalf("assets = %v, want the 2 touched assets ordered by ID", names(feed.Assets))
	}
	for _, a := range feed.Assets {
		if a.ID != ids[1] && a.ID != ids[3] {
			t.Errorf("untouched asset %q in feed", a.Name)
		}
	}
	if feed.Assets[1].Rating != 3 || feed.Assets[1].Flag == nil || *feed.Assets[1].Flag != "pick" {
		t.Errorf("feed asset does not carry current state: %+v", feed.Assets[1])
	}
}

func BenchmarkChangesSince200Assets(b *testing.B) {
	ctx := context.Background()
	idx, ids := openScanned(b, 200)
	ops := make([]sidecar.TruthOp, len(ids))
	for i, id := range ids {
		ops[i] = sidecar.TruthOp{HLC: fmt.Sprintf("019a2f001000-%04x-d", i+1), Device: "d", AssetKey: id, Field: "rating", Value: i % 6}
	}
	for start := 0; start < len(ops); start += 50 {
		if _, err := idx.ApplyOpsBatch(ctx, ops[start:start+50]); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		feed, err := idx.ChangesSince(ctx, "", 200)
		if err != nil || len(feed.Assets) != 200 {
			b.Fatalf("assets=%d err=%v", len(feed.Assets), err)
		}
	}
}
