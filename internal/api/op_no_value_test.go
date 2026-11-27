package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An op that carries no "value" member writes a cell without "v", exactly as
// the browser editor writes it for the same op; an explicit null value still
// writes "v": null. Both editors then hold byte-identical sidecars.
func TestOpWithoutValueWritesCellWithoutV(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	body := `{"ops":[
		{"hlc":"019a2f001000-0001-dev1","device":"dev1","assetKey":"` + assetID + `","field":"flag"},
		{"hlc":"019a2f001000-0002-dev1","device":"dev1","assetKey":"` + assetID + `","field":"colorLabel","value":null}
	]}`
	resp, err := http.Post(ts.URL+"/ops", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ops status=%d", resp.StatusCode)
	}
	data, err := os.ReadFile(filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.edits"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	wantFlag := "\"flag\": {\n      \"hlc\": \"019a2f001000-0001-dev1\"\n    }"
	wantLabel := "\"colorLabel\": {\n      \"hlc\": \"019a2f001000-0002-dev1\",\n      \"v\": null\n    }"
	if !strings.Contains(text, wantFlag) || !strings.Contains(text, wantLabel) {
		t.Fatalf("sidecar cells differ from the browser editor's bytes:\n%s", text)
	}
}
