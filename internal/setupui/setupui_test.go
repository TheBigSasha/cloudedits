package setupui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
)

type stubState struct {
	mu        sync.Mutex
	cfg       *config.Config
	server    ServerState
	connectFn func(ctx context.Context, key string, phase func(Phase)) (ConnectResult, error)
	changed   int
}

func (s *stubState) GetConfig() *config.Config { return s.cfg }
func (s *stubState) SaveConfig(c *config.Config) error {
	s.cfg = c
	return nil
}
func (s *stubState) PublicURL() string { return "https://library.example.ts.net" }
func (s *stubState) IndexedCount() int { return 42 }
func (s *stubState) Server(context.Context) ServerState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.server
}
func (s *stubState) Connect(ctx context.Context, key string, phase func(Phase)) (ConnectResult, error) {
	if s.connectFn == nil {
		return ConnectResult{}, nil
	}
	return s.connectFn(ctx, key, phase)
}
func (s *stubState) ConfigChanged(context.Context) {
	s.mu.Lock()
	s.changed++
	s.mu.Unlock()
}

func TestSetupUIEndpoints(t *testing.T) {
	st := &stubState{cfg: config.Default(t.TempDir())}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /setup, got %d", resp.StatusCode)
	}
	html := string(bodyBytes)
	if !strings.Contains(html, "written using AI tools") {
		t.Fatalf("expected embedded setup UI HTML to include candid AI tools security note")
	}
	if strings.Contains(strings.ToLower(html), "guarant") {
		t.Fatalf("embedded setup UI HTML must not contain guarantee wording")
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

type offlineState struct{ stubState }

func (s *offlineState) PublicURL() string { return "" }

// TestSetupUIOffersNoPhoneLinkBeforeTheNodeIsEnrolled pins that the wizard
// does not hand out a QR code or an "open" link for an address other devices
// cannot reach: until the library has a tailnet URL there is nothing to scan.
func TestSetupUIOffersNoPhoneLinkBeforeTheNodeIsEnrolled(t *testing.T) {
	st := &offlineState{stubState{cfg: config.Default(t.TempDir())}}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if payload["publicUrl"] != "" || payload["qrSvg"] != "" {
		t.Fatalf("state before enrollment: publicUrl=%q qrSvg set=%v, want both empty", payload["publicUrl"], payload["qrSvg"] != "")
	}

	page, err := http.Get(ts.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if strings.Contains(string(html), "127.0.0.1:8787") {
		t.Errorf("setup page falls back to a loopback library URL, which a phone cannot open")
	}
}
