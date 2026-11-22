//go:build !dev_apis

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseBuildDisablesDevRoutesAndCaps(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// /__dev/reset must return 404 in default release builds.
	resp, err := http.Post(ts.URL+"/__dev/reset", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on /__dev/reset in release build, got %d", resp.StatusCode)
	}

	// /capabilities must omit devApis in release builds.
	capResp, err := http.Get(ts.URL + "/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Caps map[string]any `json:"caps"`
	}
	if err := json.NewDecoder(capResp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	_ = capResp.Body.Close()
	if _, exists := body.Caps["devApis"]; exists {
		t.Fatalf("release build must not advertise devApis in /capabilities")
	}
}
