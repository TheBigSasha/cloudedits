package setupui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/muxaudit"
)

type wizardProbe struct {
	method, path, host, remote, origin, ctype, body string
	headers                                         map[string]string
}

func (p wizardProbe) do(h http.Handler) *httptest.ResponseRecorder {
	if p.method == "" {
		p.method = http.MethodGet
	}
	if p.host == "" {
		p.host = "127.0.0.1:49152"
	}
	if p.remote == "" {
		p.remote = "127.0.0.1:50000"
	}
	req := httptest.NewRequest(p.method, "http://"+p.host+p.path, strings.NewReader(p.body))
	req.Host = p.host
	req.RemoteAddr = p.remote
	if p.origin != "" {
		req.Header.Set("Origin", p.origin)
	}
	if p.ctype != "" {
		req.Header.Set("Content-Type", p.ctype)
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The wizard has no login and rewrites which folders the library serves. A
// web page the user happens to visit must not be able to drive it: browsers
// let any site send a preflight-free text/plain POST to http://127.0.0.1.
func TestWizardRefusesCrossSiteAndRemoteRequests(t *testing.T) {
	connected := false
	st := &stubState{cfg: config.Default(t.TempDir()), connectFn: func(context.Context, string, func(Phase)) (ConnectResult, error) {
		connected = true
		return ConnectResult{}, nil
	}}
	h := Handler(st)
	folder := t.TempDir()
	addFolder := `{"path":"` + folder + `"}`

	for name, p := range map[string]wizardProbe{
		"cross-site simple POST":    {method: http.MethodPost, path: "/api/folders", origin: "https://evil.example", ctype: "text/plain", body: addFolder},
		"cross-site JSON POST":      {method: http.MethodPost, path: "/api/folders", origin: "https://evil.example", ctype: "application/json", body: addFolder},
		"null origin POST":          {method: http.MethodPost, path: "/api/folders", origin: "null", ctype: "application/json", body: addFolder},
		"other local port POST":     {method: http.MethodPost, path: "/api/config", origin: "http://127.0.0.1:3000", ctype: "application/json", body: `{"hostname":"pwned"}`},
		"https look-alike":          {method: http.MethodPost, path: "/api/config", origin: "https://127.0.0.1:49152", ctype: "application/json", body: `{"hostname":"pwned"}`},
		"form POST without Origin":  {method: http.MethodPost, path: "/api/folders", ctype: "application/x-www-form-urlencoded", body: addFolder},
		"text/plain without Origin": {method: http.MethodPost, path: "/api/folders", ctype: "text/plain", body: addFolder},
		"cross-site state read":     {path: "/api/state", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}},
		"same-site state read":      {path: "/api/state", headers: map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors"}},
		"DNS-rebound state read":    {path: "/api/state", host: "rebind.evil.example:49152"},
		"DNS-rebound write": {method: http.MethodPost, path: "/api/folders", host: "rebind.evil.example:49152",
			origin: "http://rebind.evil.example:49152", ctype: "application/json", body: addFolder},
		"cross-site connect": {method: http.MethodPost, path: "/api/connect", origin: "https://evil.example", ctype: "application/json", body: `{"authKey":"tskey-auth-evil"}`},
		"text/plain connect": {method: http.MethodPost, path: "/api/connect", ctype: "text/plain", body: `{"authKey":"tskey-auth-evil"}`},
		"LAN client":         {path: "/api/state", remote: "192.168.1.50:40000", host: "192.168.1.10:49152"},
		"proxied request":    {path: "/api/state", headers: map[string]string{"X-Forwarded-For": "203.0.113.7"}},
	} {
		rec := p.do(h)
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status %d, want 403/415 (body %.120s)", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "folders") {
			t.Errorf("%s: response discloses wizard state: %.160s", name, rec.Body.String())
		}
	}
	if connected {
		t.Fatal("a refused request started the connect job")
	}
	if len(st.cfg.Folders) != 0 || st.cfg.Hostname == "pwned" {
		t.Fatalf("a refused request changed the configuration: folders=%v hostname=%q", st.cfg.Folders, st.cfg.Hostname)
	}

	// The wizard's own page keeps working.
	own := map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}
	if rec := (wizardProbe{method: http.MethodPost, path: "/api/folders", origin: "http://127.0.0.1:49152", ctype: "application/json; charset=utf-8", body: addFolder, headers: own}).do(h); rec.Code != http.StatusOK {
		t.Fatalf("same-origin add folder: status %d body %s", rec.Code, rec.Body.String())
	}
	if rec := (wizardProbe{path: "/api/state", headers: own}).do(h); rec.Code != http.StatusOK {
		t.Fatalf("same-origin state: status %d", rec.Code)
	}
	if rec := (wizardProbe{path: "/setup", host: "localhost:49152", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}}).do(h); rec.Code != http.StatusOK ||
		rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("link navigation to /setup: status %d headers %v", rec.Code, rec.Header())
	}
	if len(st.cfg.Folders) != 1 {
		t.Fatalf("expected exactly one folder after the same-origin request, got %v", st.cfg.Folders)
	}

	// Request bodies are bounded.
	huge := `{"path":"` + strings.Repeat("A", 4<<20) + `"}`
	if rec := (wizardProbe{method: http.MethodPost, path: "/api/folders", origin: "http://127.0.0.1:49152", ctype: "application/json", body: huge}).do(h); rec.Code != http.StatusBadRequest {
		t.Fatalf("4 MiB body: status %d, want 400", rec.Code)
	}
}

// Every pattern on the wizard's real router sits behind the local-only guard.
func TestWizardMuxIsFullyGuarded(t *testing.T) {
	st := &stubState{cfg: config.Default(t.TempDir())}
	got := muxaudit.Patterns(newMux(st))
	want := []string{"GET /", "GET /api/state", "GET /setup", "POST /api/config", "POST /api/connect", "POST /api/folders"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wizard routes = %q, want %q", got, want)
	}
	h := Handler(st)
	for _, pattern := range got {
		method, path, _ := strings.Cut(pattern, " ")
		rec := wizardProbe{method: method, path: path, remote: "203.0.113.7:40000", ctype: "application/json", body: "{}"}.do(h)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s answered %d to a network caller, want 403", pattern, rec.Code)
		}
	}
}
