package setupui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

func TestPickFolderBrowseAndRemoveEndpoints(t *testing.T) {
	origPick := pickFolderFunc
	t.Cleanup(func() { pickFolderFunc = origPick })

	st := &stubState{cfg: config.Default(t.TempDir())}
	ts := httptest.NewServer(Handler(st))
	defer ts.Close()

	// 1. POST /api/pick-folder returns picked path, cancelled, and unsupported cleanly.
	pickedDir := t.TempDir()
	pickFolderFunc = func(context.Context) (FolderPickResult, error) {
		return FolderPickResult{Supported: true, Cancelled: false, Path: pickedDir}, nil
	}
	resp, err := http.Post(ts.URL+"/api/pick-folder", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var pickOut FolderPickResult
	if err := json.NewDecoder(resp.Body).Decode(&pickOut); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !pickOut.Supported || pickOut.Cancelled || pickOut.Path != pickedDir {
		t.Fatalf("pick-folder picked: status=%d out=%+v", resp.StatusCode, pickOut)
	}

	pickFolderFunc = func(context.Context) (FolderPickResult, error) {
		return FolderPickResult{Supported: true, Cancelled: true}, nil
	}
	resp, err = http.Post(ts.URL+"/api/pick-folder", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	pickOut = FolderPickResult{}
	_ = json.NewDecoder(resp.Body).Decode(&pickOut)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !pickOut.Supported || !pickOut.Cancelled || pickOut.Path != "" {
		t.Fatalf("pick-folder cancelled: status=%d out=%+v", resp.StatusCode, pickOut)
	}

	pickFolderFunc = func(context.Context) (FolderPickResult, error) {
		return FolderPickResult{Supported: false}, nil
	}
	resp, err = http.Post(ts.URL+"/api/pick-folder", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	pickOut = FolderPickResult{}
	_ = json.NewDecoder(resp.Body).Decode(&pickOut)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || pickOut.Supported {
		t.Fatalf("pick-folder unsupported: status=%d out=%+v", resp.StatusCode, pickOut)
	}

	// 2. GET /api/browse-folders returns quick-access roots when path is empty,
	// and sorted subdirectories (excluding dotfiles & regular files) when path is set.
	rootsResp, err := http.Get(ts.URL + "/api/browse-folders")
	if err != nil {
		t.Fatal(err)
	}
	var browseOut BrowseResponse
	if err := json.NewDecoder(rootsResp.Body).Decode(&browseOut); err != nil {
		t.Fatal(err)
	}
	_ = rootsResp.Body.Close()
	if rootsResp.StatusCode != http.StatusOK || browseOut.Path != "" || len(browseOut.Roots) == 0 || len(browseOut.Entries) != len(browseOut.Roots) {
		t.Fatalf("browse roots: status=%d out=%+v", rootsResp.StatusCode, browseOut)
	}

	rootDir := t.TempDir()
	for _, sub := range []string{"zeta", "Alpha", ".hidden"} {
		if err := os.Mkdir(filepath.Join(rootDir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootDir, "IMG_0001.JPG"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	dirResp, err := http.Get(ts.URL + "/api/browse-folders?path=" + url.QueryEscape(rootDir))
	if err != nil {
		t.Fatal(err)
	}
	browseOut = BrowseResponse{}
	if err := json.NewDecoder(dirResp.Body).Decode(&browseOut); err != nil {
		t.Fatal(err)
	}
	_ = dirResp.Body.Close()
	if dirResp.StatusCode != http.StatusOK || browseOut.Path != rootDir || browseOut.Parent != filepath.Dir(rootDir) {
		t.Fatalf("browse dir: status=%d out=%+v", dirResp.StatusCode, browseOut)
	}
	if len(browseOut.Entries) != 2 || browseOut.Entries[0].Name != "Alpha" || browseOut.Entries[1].Name != "zeta" {
		t.Fatalf("browse entries = %+v, want [Alpha, zeta]", browseOut.Entries)
	}

	missingResp, err := http.Get(ts.URL + "/api/browse-folders?path=" + url.QueryEscape(filepath.Join(rootDir, "no-such-dir")))
	if err != nil {
		t.Fatal(err)
	}
	_ = missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("browse missing dir: status=%d, want 400", missingResp.StatusCode)
	}

	// 3. POST /api/folders/remove removes configured folders by id or path and invokes ConfigChanged.
	folderA := t.TempDir()
	folderB := t.TempDir()
	fcA, err := st.cfg.AddFolder(folderA, "Trip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.cfg.AddFolder(folderB, "Studio"); err != nil {
		t.Fatal(err)
	}

	badRem, err := http.Post(ts.URL+"/api/folders/remove", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = badRem.Body.Close()
	if badRem.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty remove: status=%d, want 400", badRem.StatusCode)
	}

	remByID, err := http.Post(ts.URL+"/api/folders/remove", "application/json", strings.NewReader(`{"id":"`+fcA.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var remPayload struct {
		OK      bool                  `json:"ok"`
		Removed bool                  `json:"removed"`
		Folders []config.FolderConfig `json:"folders"`
	}
	if err := json.NewDecoder(remByID.Body).Decode(&remPayload); err != nil {
		t.Fatal(err)
	}
	_ = remByID.Body.Close()
	if remByID.StatusCode != http.StatusOK || !remPayload.OK || !remPayload.Removed || len(remPayload.Folders) != 1 || remPayload.Folders[0].Path != folderB {
		t.Fatalf("remove by id: status=%d payload=%+v", remByID.StatusCode, remPayload)
	}

	bodyByPath, _ := json.Marshal(map[string]string{"path": folderB})
	remByPath, err := http.Post(ts.URL+"/api/folders/remove", "application/json", strings.NewReader(string(bodyByPath)))
	if err != nil {
		t.Fatal(err)
	}
	remPayload.Removed = false
	remPayload.Folders = nil
	if err := json.NewDecoder(remByPath.Body).Decode(&remPayload); err != nil {
		t.Fatal(err)
	}
	_ = remByPath.Body.Close()
	if remByPath.StatusCode != http.StatusOK || !remPayload.OK || !remPayload.Removed || len(remPayload.Folders) != 0 {
		t.Fatalf("remove by path: status=%d payload=%+v", remByPath.StatusCode, remPayload)
	}
	if st.changed != 2 {
		t.Fatalf("ConfigChanged count = %d, want 2", st.changed)
	}
}
