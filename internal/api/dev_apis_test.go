//go:build dev_apis

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDevAPIsResetFaultAndStats(t *testing.T) {
	// 1. Without isolated --sidecar-dir, /__dev/reset must refuse with 403.
	srvUnsafe, _, _ := newTestServer(t, "")
	tsUnsafe := httptest.NewServer(srvUnsafe)
	defer tsUnsafe.Close()

	refused, err := http.Post(tsUnsafe.URL+"/__dev/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = refused.Body.Close()
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 refusing reset without isolated --sidecar-dir, got %d", refused.StatusCode)
	}

	// 2. With isolated scratch --sidecar-dir, /__dev/reset, /__dev/fault, /__dev/stats all work.
	scratchDir := t.TempDir()
	srv, _, assetID := newTestServer(t, scratchDir)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Capabilities advertises devApis: true in dev_apis build.
	capResp, err := http.Get(ts.URL + "/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var capBody struct {
		Caps map[string]any `json:"caps"`
	}
	_ = json.NewDecoder(capResp.Body).Decode(&capBody)
	_ = capResp.Body.Close()
	if capBody.Caps["devApis"] != true {
		t.Fatalf("expected devApis: true in dev_apis build, got %+v", capBody.Caps)
	}

	// Write an op, check stats, inject offline fault, clear fault, reset cleanly.
	opResp, err := http.Post(ts.URL+"/ops", "application/json", strings.NewReader(
		`{"ops":[{"hlc":"019a2f001000-0001-dev1","device":"dev1","assetKey":"`+assetID+`","field":"rating","value":5}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	_ = opResp.Body.Close()

	statsResp, err := http.Get(ts.URL + "/__dev/stats")
	if err != nil {
		t.Fatal(err)
	}
	var stats map[string]any
	_ = json.NewDecoder(statsResp.Body).Decode(&stats)
	_ = statsResp.Body.Close()
	if stats["editsWriteCount"].(float64) < 1 {
		t.Fatalf("expected editsWriteCount >= 1, got %+v", stats)
	}

	// Inject offline fault -> /assets/count returns 503.
	fResp, err := http.Post(ts.URL+"/__dev/fault", "application/json", strings.NewReader(`{"offline":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = fResp.Body.Close()

	downResp, err := http.Get(ts.URL + "/assets/count")
	if err != nil {
		t.Fatal(err)
	}
	_ = downResp.Body.Close()
	if downResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 under offline fault, got %d", downResp.StatusCode)
	}

	// Reset clears fault, wipes scratch sidecars, and re-indexes source corpus cleanly.
	resetResp, err := http.Post(ts.URL+"/__dev/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resetResp.Body.Close()
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on scratch /__dev/reset, got %d", resetResp.StatusCode)
	}

	okResp, err := http.Get(ts.URL + "/assets/" + assetID)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		Rating int `json:"rating"`
	}
	_ = json.NewDecoder(okResp.Body).Decode(&asset)
	_ = okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK || asset.Rating != 0 {
		t.Fatalf("expected rating reset to 0 after /__dev/reset, got status=%d rating=%d", okResp.StatusCode, asset.Rating)
	}
}
