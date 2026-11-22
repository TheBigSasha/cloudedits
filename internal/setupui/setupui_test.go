package setupui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
)

type stubState struct {
	cfg *config.Config
}

func (s *stubState) GetConfig() *config.Config { return s.cfg }
func (s *stubState) SaveConfig(c *config.Config) error {
	s.cfg = c
	return nil
}
func (s *stubState) PublicURL() string { return "https://library.example.ts.net" }
func (s *stubState) IndexedCount() int { return 42 }

func TestSetupUIEndpoints(t *testing.T) {
	st := &stubState{cfg: config.Default(t.TempDir())}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /setup, got %d", resp.StatusCode)
	}

	folder := t.TempDir()
	addResp, err := http.Post(ts.URL+"/api/folders", "application/json", strings.NewReader(`{"path":"`+folder+`","label":"Pics"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = addResp.Body.Close()
	if addResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on POST /api/folders, got %d", addResp.StatusCode)
	}

	stateResp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.NewDecoder(stateResp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	_ = stateResp.Body.Close()
	if payload["indexedCount"].(float64) != 42 || !strings.HasPrefix(payload["qrSvg"].(string), "<svg ") {
		t.Fatalf("unexpected setup UI state: %+v", payload)
	}
}
