package thumb

import (
	"context"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"
)

func TestNegativeCacheCachesFailuresWithTTLAndLRU(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(filepath.Join(dir, "cache"), 2)
	now := time.Unix(1_800_000_000, 0)
	svc.nowFn = func() time.Time { return now }
	svc.negTTL = 5 * time.Minute
	svc.negMax = 2

	corruptPath := filepath.Join(dir, "bad.jpg")
	if err := os.WriteFile(corruptPath, []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}

	// First resolve fails and populates the negative cache.
	if _, err := svc.Resolve(context.Background(), corruptPath, "rev-1", "", "t1"); err == nil {
		t.Fatal("expected error for corrupt JPEG")
	}

	// Replace the file on disk with a valid JPEG while keeping the SAME
	// sourceRevision: the negative cache hit must return the cached failure
	// immediately without re-decoding.
	validJPEG := makeSampleJPEG(t, 400, 300, color.RGBA{R: 50, G: 180, B: 90, A: 255})
	if err := os.WriteFile(corruptPath, validJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(context.Background(), corruptPath, "rev-1", "", "t1"); err == nil {
		t.Fatal("expected cached failure on same sourceRevision within TTL")
	}

	// Priority 1 edit thumbnail still overrides a negative-cached source failure.
	editThumb := filepath.Join(dir, "bad.jpg.edit_thumbnail.jpg")
	if err := os.WriteFile(editThumb, validJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Resolve(context.Background(), corruptPath, "rev-1", editThumb, "t1")
	if err != nil || res.TierSource != "edit-thumbnail" {
		t.Fatalf("edit thumbnail should bypass negative cache: res=%+v err=%v", res, err)
	}

	// A new sourceRevision bypasses the old negative-cache entry and succeeds.
	res, err = svc.Resolve(context.Background(), corruptPath, "rev-2", "", "t1")
	if err != nil || res.TierSource != "generated" {
		t.Fatalf("new sourceRevision should succeed: res=%+v err=%v", res, err)
	}

	// Advancing past negTTL expires the cached failure for rev-1 so it retries.
	now = now.Add(6 * time.Minute)
	res, err = svc.Resolve(context.Background(), corruptPath, "rev-1", "", "t1")
	if err != nil || res.TierSource != "generated" {
		t.Fatalf("expired negative cache should retry and succeed: res=%+v err=%v", res, err)
	}

	// LRU eviction: with negMax=2, inserting a 3rd distinct failure evicts the
	// least-recently-used entry.
	svc.recordNeg("k1", errors.New("e1"))
	svc.recordNeg("k2", errors.New("e2"))
	// Touch k1 so k2 becomes least-recently-used.
	if ok, _ := svc.lookupNeg("k1"); !ok {
		t.Fatal("expected k1 hit")
	}
	svc.recordNeg("k3", errors.New("e3"))
	if ok, _ := svc.lookupNeg("k2"); ok {
		t.Fatal("expected k2 to be evicted by LRU")
	}
	if ok, _ := svc.lookupNeg("k1"); !ok {
		t.Fatal("expected k1 to survive LRU eviction")
	}
	if ok, _ := svc.lookupNeg("k3"); !ok {
		t.Fatal("expected k3 to be present")
	}

	// Transient errors must never be negative-cached.
	svc.recordNeg("k-overload", ErrOverloaded)
	svc.recordNeg("k-cancel", context.Canceled)
	svc.recordNeg("k-deadline", context.DeadlineExceeded)
	for _, k := range []string{"k-overload", "k-cancel", "k-deadline"} {
		if ok, _ := svc.lookupNeg(k); ok {
			t.Errorf("transient error key %q was negative-cached", k)
		}
	}

	// ClearCache resets the negative cache.
	if err := svc.ClearCache(); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.lookupNeg("k1"); ok {
		t.Fatal("ClearCache did not clear negative cache")
	}
}

func TestDecodeBudgetAlignsWithGOMEMLIMIT(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	// Default (when GOMEMLIMIT is unset / MaxInt64): 192 MiB budget, 48 MiB gate.
	debug.SetMemoryLimit(1<<63 - 1)
	maxB, largeB := activeDecodeBudget()
	if maxB != 192<<20 || largeB != 48<<20 {
		t.Fatalf("default budget = (%d, %d), want (%d, %d)", maxB, largeB, 192<<20, 48<<20)
	}

	// Explicit GOMEMLIMIT = 128 MiB narrows budget cleanly.
	debug.SetMemoryLimit(128 << 20)
	maxB, largeB = activeDecodeBudget()
	if maxB != 128<<20 || largeB != 32<<20 {
		t.Fatalf("128 MiB GOMEMLIMIT budget = (%d, %d), want (%d, %d)", maxB, largeB, 128<<20, 32<<20)
	}

	// Very high GOMEMLIMIT stays capped at 1 GiB.
	debug.SetMemoryLimit(4 << 30)
	maxB, _ = activeDecodeBudget()
	if maxB != 1<<30 {
		t.Fatalf("capped budget = %d, want %d", maxB, 1<<30)
	}
}
