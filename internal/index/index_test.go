package index

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

func writeTestJPEG(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	img.SetRGBA(0, 0, color.RGBA{120, 180, 240, 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanOpsIdempotencyFilterAndLosslessRebuild(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()
	dataDir := t.TempDir()

	writeTestJPEG(t, filepath.Join(photoDir, "DCIM", "A.JPG"))
	writeTestJPEG(t, filepath.Join(photoDir, "DCIM", "B.JPG"))
	// AppleDouble file must be ignored.
	writeTestJPEG(t, filepath.Join(photoDir, "DCIM", "._A.JPG"))

	folders := []config.FolderConfig{{ID: "photos", Path: photoDir, Label: "Photos"}}
	dbPath := filepath.Join(dataDir, "index.db")

	idx, err := Open(dbPath, "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}

	cnt, err := idx.Count(ctx, Filter{})
	if err != nil || cnt != 2 {
		t.Fatalf("expected 2 assets (excluding AppleDouble), got cnt=%d err=%v", cnt, err)
	}

	aidA := MakeAssetID("photos", "DCIM/A.JPG")
	ops := []sidecar.TruthOp{
		{HLC: "019a2f001000-0001-dev1", Device: "dev1", AssetKey: aidA, Field: "rating", Value: 5},
		{HLC: "019a2f001000-0002-dev1", Device: "dev1", AssetKey: aidA, Field: "flag", Value: "pick"},
		{HLC: "019a2f001000-0003-dev1", Device: "dev1", AssetKey: aidA, Field: "keywords", Value: []any{"tokyo", "night"}},
		{HLC: "019a2f001000-0004-dev1", Device: "dev1", AssetKey: aidA, Field: "recipe", Value: map[string]any{"exposure": 0.5}},
	}
	receipts, err := idx.ApplyOpsBatch(ctx, ops)
	if err != nil {
		t.Fatalf("ApplyOpsBatch: %v", err)
	}
	for _, r := range receipts {
		if r.Status != "applied" {
			t.Fatalf("expected applied receipt, got %+v", r)
		}
	}

	// Re-submitting identical (device, hlc) must be idempotent ("duplicate").
	dupReceipts, err := idx.ApplyOpsBatch(ctx, ops[:1])
	if err != nil {
		t.Fatalf("ApplyOpsBatch duplicate: %v", err)
	}
	if len(dupReceipts) != 1 || dupReceipts[0].Status != "duplicate" || dupReceipts[0].OriginalStatus != "applied" {
		t.Fatalf("expected duplicate/applied receipt, got %+v", dupReceipts)
	}

	// Filter by rating_gte + flag + repeatable keywords AND.
	minRating := 4
	list, total, err := idx.Range(ctx, Filter{
		RatingGte: &minRating,
		Flag:      "pick",
		Keywords:  []string{"tokyo", "night"},
	}, 0, 50)
	if err != nil || total != 1 || len(list) != 1 || list[0].ID != aidA {
		t.Fatalf("expected filtered match for A.JPG, got total=%d list=%+v err=%v", total, list, err)
	}

	// Changes feed must contain all 4 ops.
	feed, err := idx.ChangesSince(ctx, "0", 50)
	if err != nil || len(feed.Ops) != 4 {
		t.Fatalf("expected 4 ops in changes feed, got %d err=%v", len(feed.Ops), err)
	}

	// Lossless rebuild law: close DB, DELETE index.db, reopen, rescan -> 100% state restored from .hdrtoys/*.edits!
	_ = idx.Close()
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")

	idx2, err := Open(dbPath, "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatalf("reopen after delete: %v", err)
	}
	defer func() { _ = idx2.Close() }()
	if err := idx2.ScanAll(ctx); err != nil {
		t.Fatalf("rescan after delete: %v", err)
	}

	a2, err := idx2.GetAsset(ctx, aidA)
	if err != nil {
		t.Fatalf("GetAsset after rebuild: %v", err)
	}
	if a2.Rating != 5 || a2.Flag == nil || *a2.Flag != "pick" || !a2.EditedByHdrToys || len(a2.Keywords) != 2 {
		t.Fatalf("lossless rebuild lost metadata: %+v", a2)
	}
}

func TestConcurrentApplyOpsBatchRaceAndCoalescing(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()
	dataDir := t.TempDir()

	const numAssets = 8
	assetIDs := make([]string, numAssets)
	for i := 0; i < numAssets; i++ {
		rel := fmt.Sprintf("IMG_%02d.JPG", i)
		writeTestJPEG(t, filepath.Join(photoDir, rel))
		assetIDs[i] = MakeAssetID("photos", rel)
	}

	folders := []config.FolderConfig{{ID: "photos", Path: photoDir, Label: "Photos"}}
	idx, err := Open(filepath.Join(dataDir, "index.db"), "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = idx.Close() }()
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}

	const writers = 8
	const batchesPerWriter = 10
	var wg sync.WaitGroup
	errCh := make(chan error, writers*batchesPerWriter)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			dev := fmt.Sprintf("dev%d", worker)
			for b := 0; b < batchesPerWriter; b++ {
				target := assetIDs[(worker+b)%numAssets]
				baseMs := 0x019a2f001000 + b*100 + worker
				ops := []sidecar.TruthOp{
					{HLC: fmt.Sprintf("%012x-0001-%s", baseMs, dev), Device: dev, AssetKey: target, Field: "rating", Value: (b % 5) + 1},
					{HLC: fmt.Sprintf("%012x-0002-%s", baseMs, dev), Device: dev, AssetKey: target, Field: "flag", Value: "pick"},
					{HLC: fmt.Sprintf("%012x-0003-%s", baseMs, dev), Device: dev, AssetKey: target, Field: "colorLabel", Value: "blue"},
					{HLC: fmt.Sprintf("%012x-0004-%s", baseMs, dev), Device: dev, AssetKey: target, Field: "keywords", Value: []any{fmt.Sprintf("w%d", worker), fmt.Sprintf("b%d", b)}},
				}
				receipts, aerr := idx.ApplyOpsBatch(ctx, ops)
				if aerr != nil {
					errCh <- fmt.Errorf("worker %d batch %d: %w", worker, b, aerr)
					return
				}
				if len(receipts) != len(ops) {
					errCh <- fmt.Errorf("worker %d batch %d: got %d receipts, want %d", worker, b, len(receipts), len(ops))
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Verify SQLite state matches disk sidecar verdicts for every asset.
	for i, aid := range assetIDs {
		a, err := idx.GetAsset(ctx, aid)
		if err != nil {
			t.Fatalf("GetAsset(%s): %v", aid, err)
		}
		res, err := idx.Sidecars().Read(a.FolderRoot, a.SidecarRoot, a.RelPath)
		if err != nil || res.Sidecar == nil {
			t.Fatalf("Read sidecar %d: res=%+v err=%v", i, res, err)
		}
		rating, flag, label, _, _ := sidecar.ExtractVerdicts(res.Sidecar)
		if a.Rating != rating {
			t.Fatalf("asset %d SQLite rating=%d != sidecar rating=%d", i, a.Rating, rating)
		}
		gotFlag := ""
		if a.Flag != nil {
			gotFlag = *a.Flag
		}
		if gotFlag != flag {
			t.Fatalf("asset %d SQLite flag=%q != sidecar flag=%q", i, gotFlag, flag)
		}
		gotLabel := ""
		if a.ColorLabel != nil {
			gotLabel = *a.ColorLabel
		}
		if gotLabel != label {
			t.Fatalf("asset %d SQLite label=%q != sidecar label=%q", i, gotLabel, label)
		}
	}
}
