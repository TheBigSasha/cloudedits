package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

// outsideEdit does what another program with direct access to the folder
// does: read the asset's canonical sidecar, merge one op into it, and replace
// the file. The file's modification time is moved forward explicitly so the
// test does not depend on the filesystem's timestamp resolution.
func outsideEdit(t *testing.T, idx *Index, name string, op sidecar.TruthOp) {
	t.Helper()
	fc := idx.Folders()[0]
	root := sidecar.SidecarRoot(fc.Path, fc.ID, "")
	target, err := sidecar.CanonicalEditsPath(root, name)
	if err != nil {
		t.Fatal(err)
	}
	sc := sidecar.Empty(name, nil, "")
	if data, err := os.ReadFile(target); err == nil {
		if sc, err = sidecar.Parse(data); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sidecar.ApplyOp(sc, op); err != nil {
		t.Fatal(err)
	}
	payload, err := sidecar.Serialize(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMTime(t, target)
}

var mtimeStep time.Duration

// bumpMTime gives path a modification time later than any this test set
// before.
func bumpMTime(t *testing.T, path string) {
	t.Helper()
	mtimeStep += 2 * time.Second
	when := time.Now().Add(mtimeStep)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func feedFrom(t *testing.T, idx *Index, cursor string) *ChangesFeed {
	t.Helper()
	feed, err := idx.ChangesSince(context.Background(), cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	return feed
}

func headCursor(t *testing.T, idx *Index) string {
	t.Helper()
	feed := feedFrom(t, idx, HeadCursor)
	if len(feed.Ops) != 0 || feed.SnapshotRequired {
		t.Fatalf("since=head must be an empty page, got %+v", feed)
	}
	return feed.NextCursor
}

func describe(ops []ChangeOp) string {
	parts := make([]string, len(ops))
	for i, op := range ops {
		parts[i] = fmt.Sprintf("%s:%s", op.Outcome, op.Field)
	}
	return strings.Join(parts, ",")
}

// An edit another program writes straight into the folder must reach the
// change feed: without an entry a client polling GET /changes shows the old
// rating until it reloads, although the asset list already has the new one.
func TestScanJournalsASidecarEditMadeOutsideTheServer(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 2)
	cursor := headCursor(t, idx)

	outsideEdit(t, idx, "IMG_0001.jpg", sidecar.TruthOp{
		HLC: "019a2f005000-0001-browser", Device: "browser", Field: "rating", Value: 4,
	})
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	feed := feedFrom(t, idx, cursor)
	if len(feed.Ops) != 1 {
		t.Fatalf("feed after an outside edit = [%s], want one observed sidecar entry", describe(feed.Ops))
	}
	entry := feed.Ops[0]
	if entry.Outcome != OutcomeObserved || entry.Field != FieldSidecar || entry.AssetID != ids[1] {
		t.Fatalf("entry = %+v, want observed sidecar entry for %s", entry, ids[1])
	}
	if entry.HLC != "019a2f005000-0001-browser" {
		t.Errorf("entry hlc = %q, want the newest HLC in the sidecar so clients can order their next edit after it", entry.HLC)
	}
	if len(feed.Assets) != 1 || feed.Assets[0].ID != ids[1] || feed.Assets[0].Rating != 4 {
		t.Fatalf("feed assets = %+v, want the edited asset with rating 4", feed.Assets)
	}

	// The same state seen again is not a change.
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if again := feedFrom(t, idx, feed.NextCursor); len(again.Ops) != 0 {
		t.Fatalf("rescan of an unchanged folder journaled [%s]", describe(again.Ops))
	}
}

// Ops applied through POST /ops rewrite the sidecar too. A scan that follows
// must not report the server's own write as an outside edit, or every op would
// reach clients twice and the second time as "re-read everything".
func TestScanDoesNotJournalTheServersOwnWrites(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 3)
	cursor := headCursor(t, idx)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = idx.ScanAll(ctx)
			}
		}
	}()
	const rounds = 60
	for i := 0; i < rounds; i++ {
		op := sidecar.TruthOp{
			HLC: fmt.Sprintf("019a2f006000-%04x-devA", i+1), Device: "devA", AssetKey: ids[i%3], Field: "rating", Value: i % 6,
		}
		if _, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{op}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	feed, err := idx.ChangesSince(ctx, cursor, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range feed.Ops {
		if op.Outcome != outcomeApplied {
			t.Fatalf("the server's own write was journaled as %s:%s (feed: [%s])", op.Outcome, op.Field, describe(feed.Ops))
		}
	}
	if len(feed.Ops) != rounds {
		t.Fatalf("feed carries %d ops, want the %d applied", len(feed.Ops), rounds)
	}
}

// POST /ops merges the sidecar on disk before it applies its ops. When that
// file already holds an edit another program made, the write absorbs it and
// the next scan sees nothing new; the entry has to be journaled by the write.
func TestOpsBatchJournalsAnOutsideEditItAbsorbed(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 1)
	if _, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-devA", Device: "devA", AssetKey: ids[0], Field: "rating", Value: 1},
	}); err != nil {
		t.Fatal(err)
	}
	cursor := headCursor(t, idx)

	outsideEdit(t, idx, "IMG_0000.jpg", sidecar.TruthOp{
		HLC: "019a2f005000-0001-browser", Device: "browser", Field: "colorLabel", Value: "red",
	})
	if _, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
		{HLC: "019a2f006000-0001-devA", Device: "devA", AssetKey: ids[0], Field: "flag", Value: "pick"},
	}); err != nil {
		t.Fatal(err)
	}

	feed := feedFrom(t, idx, cursor)
	if got := describe(feed.Ops); got != "observed:sidecar,applied:flag" {
		t.Fatalf("feed = [%s], want the absorbed outside edit ahead of the op", got)
	}
	asset := feed.Assets[0]
	if asset.ColorLabel == nil || *asset.ColorLabel != "red" || asset.Flag == nil || *asset.Flag != "pick" {
		t.Fatalf("feed asset = %+v, want both the outside label and the op's flag", asset)
	}
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if again := feedFrom(t, idx, feed.NextCursor); len(again.Ops) != 0 {
		t.Fatalf("the absorbed edit was journaled a second time by the scan: [%s]", describe(again.Ops))
	}
}

// A sidecar that is already there when its photo is first indexed is the
// asset's starting state, not a change: a rebuilt index must not replay every
// sidecar of the library into the feed.
func TestFirstIndexingOfAnAssetJournalsNothing(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()
	writeTestJPEGFile(t, filepath.Join(photoDir, "A.jpg"))
	folders := []config.FolderConfig{{ID: "p", Path: photoDir, Label: "P"}}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	idx, err := Open(dbPath, "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	outsideEdit(t, idx, "A.jpg", sidecar.TruthOp{HLC: "019a2f005000-0001-browser", Device: "browser", Field: "rating", Value: 4})
	_ = idx.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}

	rebuilt, err := Open(dbPath, "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rebuilt.Close() })
	if err := rebuilt.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if feed := feedFrom(t, rebuilt, ""); len(feed.Ops) != 0 {
		t.Fatalf("a rebuilt index journaled [%s] for sidecars that were already there", describe(feed.Ops))
	}
	asset, err := rebuilt.GetAsset(ctx, MakeAssetID("p", "A.jpg"))
	if err != nil || asset.Rating != 4 {
		t.Fatalf("rebuilt projection: %+v err=%v", asset, err)
	}

	// The baseline it took is what later outside edits are compared with.
	cursor := headCursor(t, rebuilt)
	outsideEdit(t, rebuilt, "A.jpg", sidecar.TruthOp{HLC: "019a2f007000-0001-browser", Device: "browser", Field: "rating", Value: 2})
	if err := rebuilt.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := describe(feedFrom(t, rebuilt, cursor).Ops); got != "observed:sidecar" {
		t.Fatalf("feed after an outside edit on a rebuilt index = [%s]", got)
	}
}

// Removing the sidecar resets the asset. No op expresses that, so the feed
// must still say the asset changed.
func TestScanJournalsARemovedSidecar(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 1)
	if _, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-devA", Device: "devA", AssetKey: ids[0], Field: "rating", Value: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	cursor := headCursor(t, idx)

	fc := idx.Folders()[0]
	target, _ := sidecar.CanonicalEditsPath(sidecar.SidecarRoot(fc.Path, fc.ID, ""), "IMG_0000.jpg")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	feed := feedFrom(t, idx, cursor)
	if got := describe(feed.Ops); got != "observed:sidecar" {
		t.Fatalf("feed after removing the sidecar = [%s]", got)
	}
	if feed.Ops[0].HLC == "" || feed.Assets[0].Rating != 0 {
		t.Fatalf("entry=%+v asset rating=%d, want a well-formed entry and the reset asset", feed.Ops[0], feed.Assets[0].Rating)
	}
}

// The edited preview is what every other client shows as the photo's
// thumbnail. Replacing it (through the API, or by a program writing the file)
// must reach the feed and must change the version in the thumbnail URL.
func TestEditedPreviewChangesAreJournaledAndVersioned(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 1)
	asset, err := idx.GetAsset(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	plain := asset.ThumbnailVersion
	if plain == "" {
		t.Fatal("an asset without an edited preview still needs a thumbnail version")
	}
	cursor := headCursor(t, idx)

	// Through the API.
	if _, err := idx.Sidecars().WriteEditThumbnail(asset.SidecarRoot, asset.RelPath, []byte("\xff\xd8\xff first")); err != nil {
		t.Fatal(err)
	}
	if err := idx.NoteEditThumbnail(ctx, asset); err != nil {
		t.Fatal(err)
	}
	feed := feedFrom(t, idx, cursor)
	if got := describe(feed.Ops); got != "observed:editThumbnail" {
		t.Fatalf("feed after an uploaded preview = [%s]", got)
	}
	uploaded := feed.Assets[0].ThumbnailVersion
	if uploaded == plain {
		t.Fatalf("thumbnail version %q did not change when the edited preview appeared", uploaded)
	}
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if again := feedFrom(t, idx, feed.NextCursor); len(again.Ops) != 0 {
		t.Fatalf("the scan journaled the uploaded preview again: [%s]", describe(again.Ops))
	}

	// By another program writing the file.
	thumbPath, _ := sidecar.EditThumbnailPath(asset.SidecarRoot, asset.RelPath)
	if err := os.WriteFile(thumbPath, []byte("\xff\xd8\xff second, longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpMTime(t, thumbPath)
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	outside := feedFrom(t, idx, feed.NextCursor)
	if got := describe(outside.Ops); got != "observed:editThumbnail" {
		t.Fatalf("feed after a preview written into the folder = [%s]", got)
	}
	if v := outside.Assets[0].ThumbnailVersion; v == uploaded || v == plain {
		t.Fatalf("thumbnail version %q did not change when the edited preview was replaced", v)
	}
}

// An index written before the journal kept per-asset baselines has none. The
// first outside edit after the upgrade must still be reported, and edited
// previews that were already there must not be.
func TestOutsideEditsAreJournaledWithoutABaseline(t *testing.T) {
	ctx := context.Background()
	idx, ids := openScanned(t, 2)
	asset, _ := idx.GetAsset(ctx, ids[0])
	if _, err := idx.Sidecars().WriteEditThumbnail(asset.SidecarRoot, asset.RelPath, []byte("\xff\xd8\xff old")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(`DELETE FROM asset_feed_state`); err != nil {
		t.Fatal(err)
	}
	cursor := headCursor(t, idx)

	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if feed := feedFrom(t, idx, cursor); len(feed.Ops) != 0 {
		t.Fatalf("taking the first baseline journaled [%s]", describe(feed.Ops))
	}

	if _, err := idx.db.Exec(`DELETE FROM asset_feed_state`); err != nil {
		t.Fatal(err)
	}
	outsideEdit(t, idx, "IMG_0001.jpg", sidecar.TruthOp{HLC: "019a2f005000-0001-browser", Device: "browser", Field: "rating", Value: 3})
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := describe(feedFrom(t, idx, cursor).Ops); got != "observed:sidecar" {
		t.Fatalf("feed after the first outside edit without a baseline = [%s]", got)
	}
}

// The journal is bounded. A client whose cursor points at entries that were
// removed cannot be caught up from the feed, and has to be told so instead of
// silently receiving a feed with a hole in it.
func TestJournalRetentionRequiresSnapshotForACursorBehindIt(t *testing.T) {
	ctx := context.Background()
	retain := journalRetainRows
	journalRetainRows = 8
	t.Cleanup(func() { journalRetainRows = retain })

	idx, ids := openScanned(t, 1)
	apply := func(n int) {
		t.Helper()
		if _, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
			{HLC: fmt.Sprintf("019a2f001000-%04x-devA", n), Device: "devA", AssetKey: ids[0], Field: "rating", Value: n % 6},
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply(1)
	early := feedFrom(t, idx, "").NextCursor
	for n := 2; n <= 40; n++ {
		apply(n)
	}

	var rows, oldest int64
	if err := idx.db.QueryRow(`SELECT COUNT(*), MIN(seq) FROM ops_log`).Scan(&rows, &oldest); err != nil {
		t.Fatal(err)
	}
	if limit := journalRetainRows + journalPruneSlack(); rows > limit {
		t.Fatalf("journal holds %d rows, want at most %d", rows, limit)
	}

	behind := feedFrom(t, idx, early)
	head := headCursor(t, idx)
	if !behind.SnapshotRequired || len(behind.Ops) != 0 || behind.NextCursor != head {
		t.Fatalf("cursor behind the retained journal: %+v, want snapshotRequired with the head %q", behind, head)
	}

	// A cursor inside the retained range still reads exactly what follows it.
	recent := feedFrom(t, idx, "")
	if recent.SnapshotRequired || len(recent.Ops) == 0 || recent.Ops[0].Seq != oldest {
		t.Fatalf("bootstrap over a trimmed journal: snapshotRequired=%v first=%+v, want the oldest retained entry %d",
			recent.SnapshotRequired, recent.Ops, oldest)
	}
	apply(41)
	next := feedFrom(t, idx, head)
	if next.SnapshotRequired || len(next.Ops) != 1 || next.Ops[0].HLC != "019a2f001000-0029-devA" {
		t.Fatalf("poll from the head after one more op: %+v", next)
	}

	// The journal also answers "already processed". An op replayed after its
	// row was removed is evaluated again: one that is still the state on disk
	// is applied, not reported as having lost to something.
	receipts, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-devA", Device: "devA", AssetKey: ids[0], Field: "rating", Value: 1},
		{HLC: "019a2f002000-0001-devB", Device: "devB", AssetKey: ids[0], Field: "flag", Value: "pick"},
	})
	if err != nil || receipts[0].Status != "stale" || receipts[1].Status != "applied" {
		t.Fatalf("replay of a superseded op + a new op: %+v err=%v", receipts, err)
	}
	var removed int
	if err := idx.db.QueryRow(`SELECT COUNT(*) FROM ops_log WHERE device = 'devB'`).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(`DELETE FROM ops_log WHERE device = 'devB'`); err != nil || removed != 1 {
		t.Fatalf("removing devB's row: removed=%d err=%v", removed, err)
	}
	receipts, err = idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
		{HLC: "019a2f002000-0001-devB", Device: "devB", AssetKey: ids[0], Field: "flag", Value: "pick"},
	})
	if err != nil || receipts[0].Status != "applied" {
		t.Fatalf("replay of the op that is the state on disk, after its row was removed: %+v err=%v", receipts, err)
	}

	// The floor survives a restart.
	dbPath, folders := idx.dbPath, idx.Folders()
	_ = idx.Close()
	reopened, err := Open(dbPath, "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if again := feedFrom(t, reopened, early); !again.SnapshotRequired {
		t.Fatalf("after a restart the trimmed cursor reads %+v, want snapshotRequired", again)
	}
	if kept := feedFrom(t, reopened, next.NextCursor); kept.SnapshotRequired {
		t.Fatal("a restart invalidated a cursor inside the retained journal")
	}
}

// A cursor belongs to the journal that issued it. After the index file is
// deleted and rebuilt, positions restart; a cursor from the old journal that
// happens to be a valid number in the new one must not be read as a position.
func TestCursorOfAnotherJournalRequiresSnapshot(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()
	writeTestJPEGFile(t, filepath.Join(photoDir, "A.jpg"))
	folders := []config.FolderConfig{{ID: "p", Path: photoDir, Label: "P"}}
	dbPath := filepath.Join(t.TempDir(), "index.db")
	open := func() *Index {
		t.Helper()
		idx, err := Open(dbPath, "", folders, sidecar.NewStore())
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.ScanAll(ctx); err != nil {
			t.Fatal(err)
		}
		return idx
	}
	apply := func(idx *Index, n int) {
		t.Helper()
		if _, err := idx.ApplyOpsBatch(ctx, []sidecar.TruthOp{
			{HLC: fmt.Sprintf("019a2f001000-%04x-devA", n), Device: "devA", AssetKey: MakeAssetID("p", "A.jpg"), Field: "rating", Value: n % 6},
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := open()
	apply(first, 1)
	old := feedFrom(t, first, "").NextCursor
	_ = first.Close()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}

	second := open()
	t.Cleanup(func() { _ = second.Close() })
	apply(second, 2)
	apply(second, 3)
	apply(second, 4)

	feed := feedFrom(t, second, old)
	if !feed.SnapshotRequired || len(feed.Ops) != 0 {
		t.Fatalf("a cursor of the deleted journal read %+v, want snapshotRequired", feed)
	}
	if after := feedFrom(t, second, feed.NextCursor); after.SnapshotRequired || len(after.Ops) != 0 {
		t.Fatalf("the cursor handed back with snapshotRequired is not the head: %+v", after)
	}
	for _, junk := range []string{"0", "c3", "not-a-cursor", "." + strings.Repeat("9", 30)} {
		if got := feedFrom(t, second, junk); !got.SnapshotRequired {
			t.Errorf("since=%q read %+v, want snapshotRequired", junk, got)
		}
	}
}
