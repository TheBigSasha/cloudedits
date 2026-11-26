package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/index"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

func postRating(t *testing.T, srv *Server, assetID string, rating int) (status string) {
	t.Helper()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	body := strings.NewReader(`{"rating":` + string(rune('0'+rating)) + `}`)
	resp, err := http.Post(ts.URL+"/assets/"+assetID+"/rating", "application/json", body)
	if err != nil {
		t.Fatalf("POST rating: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK       bool `json:"ok"`
		Receipts []struct {
			Status string `json:"status"`
		} `json:"receipts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || len(out.Receipts) != 1 {
		t.Fatalf("POST rating: status=%d err=%v body=%+v", resp.StatusCode, err, out)
	}
	return out.Receipts[0].Status
}

// The legacy rating endpoint stamps writes from a clock that restarts at
// counter 0 with every process. After a restart its first HLCs repeated ones
// already journaled, so the write was answered `ok: true, durable: true` while
// its receipt said "duplicate" and the rating never changed.
func TestLegacyRatingWriteAppliesAfterRestart(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	if status := postRating(t, srv, assetID, 3); status != "applied" {
		t.Fatalf("first rating: receipt status %q", status)
	}

	// A restart: same index and sidecars, fresh server clocks.
	restarted, err := New(srv.opts)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if status := postRating(t, restarted, assetID, 5); status != "applied" {
		t.Fatalf("rating after restart: receipt status %q, want applied", status)
	}
	asset, err := restarted.idx.GetAsset(t.Context(), assetID)
	if err != nil {
		t.Fatal(err)
	}
	if asset.Rating != 5 {
		t.Fatalf("rating after restart = %d, want 5", asset.Rating)
	}
}

// The index is disposable. After it is rebuilt nothing is journaled, but the
// sidecar still carries the old v1-stamped cell, which a counter restarted at
// zero would tie with or lose to.
func TestLegacyRatingWriteAppliesAfterIndexRebuild(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	for _, rating := range []int{2, 3} {
		if status := postRating(t, srv, assetID, rating); status != "applied" {
			t.Fatalf("rating %d: receipt status %q", rating, status)
		}
	}
	rebuilt, err := index.Open(filepath.Join(t.TempDir(), "index.db"), "", srv.idx.Folders(), sidecar.NewStore())
	if err != nil {
		t.Fatalf("rebuild index: %v", err)
	}
	t.Cleanup(func() { _ = rebuilt.Close() })
	if err := rebuilt.ScanAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts := srv.opts
	opts.Index = rebuilt
	restarted, err := New(opts)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if status := postRating(t, restarted, assetID, 1); status != "applied" {
		t.Fatalf("rating after index rebuild: receipt status %q, want applied", status)
	}
	if asset, err := rebuilt.GetAsset(t.Context(), assetID); err != nil || asset.Rating != 1 {
		t.Fatalf("rating after index rebuild = %+v (err=%v), want 1", asset, err)
	}
}
