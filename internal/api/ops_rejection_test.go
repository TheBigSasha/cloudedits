package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// POST /ops answers 200 with one receipt per op. An op that cannot be applied
// is reported as "rejected" with a reason; it does not turn the whole batch
// into a 404/400 that leaves the client's valid ops unacknowledged forever.
func TestPostOpsReportsRejectedOpsPerReceipt(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := `{"ops":[
		{"hlc":"019a2f001000-0001-devA","device":"devA","assetKey":"` + assetID + `","field":"rating","value":4},
		{"hlc":"019a2f001000-0002-devA","device":"devA","assetKey":"bm8tc3VjaC1hc3NldA","field":"rating","value":2},
		{"hlc":"019a2f001000-0003-devA","device":"devA","assetKey":"` + assetID + `","field":"variant","value":{"name":"no id"}}
	]}`
	resp, err := http.Post(ts.URL+"/ops", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Durable  bool `json:"durable"`
		Applied  int  `json:"applied"`
		Rejected int  `json:"rejected"`
		Receipts []struct {
			HLC    string `json:"hlc"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"receipts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ops: status=%d err=%v", resp.StatusCode, err)
	}
	if out.Applied != 1 || out.Rejected != 2 || len(out.Receipts) != 3 {
		t.Fatalf("applied=%d rejected=%d receipts=%+v", out.Applied, out.Rejected, out.Receipts)
	}
	if out.Receipts[0].Status != "applied" ||
		out.Receipts[1].Status != "rejected" || out.Receipts[1].Reason != "asset-not-found" ||
		out.Receipts[2].Status != "rejected" || out.Receipts[2].Reason != "invalid-op" {
		t.Fatalf("receipts = %+v", out.Receipts)
	}
	if asset, err := srv.idx.GetAsset(t.Context(), assetID); err != nil || asset.Rating != 4 {
		t.Fatalf("valid op in the batch was not applied: %+v err=%v", asset, err)
	}

	// The one-asset endpoints still answer with the asset's own HTTP status.
	ratingResp, err := http.Post(ts.URL+"/assets/bm8tc3VjaC1hc3NldA/rating", "application/json", strings.NewReader(`{"rating":3}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = ratingResp.Body.Close()
	if ratingResp.StatusCode != http.StatusNotFound {
		t.Fatalf("rating an unknown asset: status %d, want 404", ratingResp.StatusCode)
	}
}

// An op stamped far ahead of the server's clock would win last-writer-wins
// against every edit any device makes until that time arrives. It is refused
// on its own receipt, leaves no trace in the sidecar or the journal, does not
// move the server's clock, and does not block the op next to it.
func TestPostOpsRejectsAnOpBeyondTheClockDriftBound(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := `[
		{"hlc":"1fffffffffffff-ffffffff-z","device":"z","assetKey":"` + assetID + `","field":"rating","value":5},
		{"hlc":"019a2f001000-0001-devA","device":"devA","assetKey":"` + assetID + `","field":"rating","value":2}
	]`
	resp, err := http.Post(ts.URL+"/ops", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Receipts []struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"receipts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || len(out.Receipts) != 2 {
		t.Fatalf("POST /ops: status=%d err=%v receipts=%+v", resp.StatusCode, err, out.Receipts)
	}
	if out.Receipts[0].Status != "rejected" || out.Receipts[0].Reason != "clock-skew" || out.Receipts[1].Status != "applied" {
		t.Fatalf("receipts = %+v, want [rejected clock-skew, applied]", out.Receipts)
	}
	if asset, err := srv.idx.GetAsset(t.Context(), assetID); err != nil || asset.Rating != 2 {
		t.Fatalf("rating = %+v err=%v, want the in-bound op's 2", asset, err)
	}
	if feed, err := srv.idx.ChangesSince(t.Context(), "", 0); err != nil || len(feed.Ops) != 1 {
		t.Fatalf("journal = %+v err=%v, want only the applied op", feed, err)
	}
	// The next server-issued HLC is near the present, not at the refused one.
	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/assets/"+assetID+"/variants/none", nil)
	delResp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	var delOut struct {
		Receipts []struct {
			HLC string `json:"hlc"`
		} `json:"receipts"`
	}
	_ = json.NewDecoder(delResp.Body).Decode(&delOut)
	_ = delResp.Body.Close()
	if len(delOut.Receipts) != 1 || strings.HasPrefix(delOut.Receipts[0].HLC, "1fffffffffffff") {
		t.Fatalf("server clock after the refused op issued %+v", delOut.Receipts)
	}
}
