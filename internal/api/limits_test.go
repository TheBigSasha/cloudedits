package api

import (
	"net/http"
	"strings"
	"testing"
)

// The JSON decoder buffers a whole top-level value before unmarshalling it,
// so an unbounded body is an unbounded allocation. Small-JSON routes must cap
// what they read.
func TestRatingBodyIsBounded(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	huge := `{"pad":"` + strings.Repeat("A", 4<<20) + `","rating":3}`
	rec := probe{method: http.MethodPost, path: "/assets/" + assetID + "/rating", ctype: "application/json", body: huge}.do(srv)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("4 MiB rating body: status %d, want 400 (body %.120s)", rec.Code, rec.Body.String())
	}
	rec = probe{method: http.MethodPost, path: "/assets/" + assetID + "/rating", ctype: "application/json", body: `{"rating":3}`}.do(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("normal rating body: status %d body %s", rec.Code, rec.Body.String())
	}
}
