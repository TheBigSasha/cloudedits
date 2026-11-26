package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/auth"
)

type probe struct {
	method, path, origin, host, remote, ctype, body string
	headers                                         map[string]string
}

func (p probe) do(srv http.Handler) *httptest.ResponseRecorder {
	if p.method == "" {
		p.method = http.MethodGet
	}
	if p.remote == "" {
		p.remote = "127.0.0.1:55555"
	}
	req := httptest.NewRequest(p.method, "http://127.0.0.1:8787"+p.path, strings.NewReader(p.body))
	req.RemoteAddr = p.remote
	if p.host != "" {
		req.Host = p.host
	}
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
	srv.ServeHTTP(rec, req)
	return rec
}

// A page on any website the owner visits is cross-origin to the library but
// its requests arrive from the owner's machine. It must not be able to read
// the library, mint an owner session, or write edits.
func TestForeignOriginCannotBorrowAmbientAuth(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	editsPath := filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.edits")
	op := `[{"hlc":"019a2f0012ab-0001-evil","device":"evil","assetKey":"` + assetID + `","field":"rating","value":1}]`

	for _, origin := range []string{"https://evil.example", "null", "https://hdr.toys.evil.example", "http://hdr.toys", "https://hdr.toys:8443", "file://", "evil"} {
		for _, p := range []probe{
			{path: "/assets"},
			{path: "/capabilities"},
			{path: "/assets/" + assetID + "/file"},
			{method: http.MethodPost, path: "/auth/session"},
			{method: http.MethodPost, path: "/ops", ctype: "text/plain", body: op},
			{method: http.MethodPost, path: "/assets/" + assetID + "/rating", ctype: "text/plain", body: `{"rating":1}`},
			{method: http.MethodPut, path: "/assets/" + assetID + "/edit-thumbnail", body: "\xff\xd8\xff"},
			{method: http.MethodOptions, path: "/ops", headers: map[string]string{"Access-Control-Request-Method": "POST"}},
		} {
			p.origin = origin
			rec := p.do(srv)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Origin %q %s %s: status %d, want 403 (body %.120s)", origin, p.method, p.path, rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("Origin %q %s %s: Access-Control-Allow-Origin %q leaked to a foreign origin", origin, p.method, p.path, got)
			}
			if strings.Contains(rec.Body.String(), "ses_") || strings.Contains(rec.Body.String(), "IMG_0001") {
				t.Fatalf("Origin %q %s %s: response discloses library data: %.160s", origin, p.method, p.path, rec.Body.String())
			}
		}
	}
	if _, err := os.Stat(editsPath); !os.IsNotExist(err) {
		t.Fatalf("a cross-site request wrote %s (stat err=%v)", editsPath, err)
	}

	// The configured editor origin keeps working, with CORS scoped to it.
	rec := probe{path: "/assets", origin: "https://hdr.toys"}.do(srv)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "https://hdr.toys" {
		t.Fatalf("editor origin: status %d ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Origin") {
		t.Fatalf("editor origin response lacks Vary: Origin")
	}
	rec = probe{method: http.MethodPost, path: "/ops", origin: "https://hdr.toys", ctype: "application/json", body: op}.do(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("editor origin POST /ops: status %d body %s", rec.Code, rec.Body.String())
	}
	// Non-browser clients send no Origin and are unaffected, with no CORS grant.
	rec = probe{path: "/assets"}.do(srv)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no-Origin request: status %d ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	// A locally served editor may reach a loopback listener...
	rec = probe{path: "/assets", origin: "http://localhost:3000"}.do(srv)
	if rec.Code != http.StatusOK || rec.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3000" {
		t.Fatalf("loopback origin on loopback: status %d ACAO %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	// ...but a loopback origin is not trusted for requests arriving over the network.
	rec = probe{path: "/assets", origin: "http://localhost:3000", remote: "100.64.0.9:51234", headers: map[string]string{"Authorization": "Bearer tok-123"}}.do(srv)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("loopback origin over the network: status %d, want 403", rec.Code)
	}
}

// Every response carries nosniff, and Private Network Access preflights from
// an allowed origin are acknowledged.
func TestNosniffAndPrivateNetworkPreflight(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	for _, p := range []probe{
		{path: "/capabilities"},
		{path: "/assets/" + assetID + "/file"},
		{path: "/assets/" + assetID + "/blob?tier=t1"},
		{path: "/assets/missing/file"},
		{path: "/nope"},
		{path: "/assets", origin: "https://evil.example"},
		{method: http.MethodOptions, path: "/ops", origin: "https://hdr.toys"},
	} {
		if got := p.do(srv).Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s %s: X-Content-Type-Options = %q, want nosniff", p.method, p.path, got)
		}
	}
	rec := probe{method: http.MethodOptions, path: "/ops", origin: "https://hdr.toys", headers: map[string]string{
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Private-Network": "true",
	}}.do(srv)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Private-Network") != "true" ||
		rec.Header().Get("Access-Control-Allow-Origin") != "https://hdr.toys" {
		t.Fatalf("PNA preflight: status %d headers %v", rec.Code, rec.Header())
	}
}

// Loopback trust must not extend to DNS-rebound requests (foreign Host) or to
// requests relayed by a local reverse proxy; explicit tokens still work.
func TestLoopbackTrustRequiresLoopbackHostAndDirectConnection(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	for name, p := range map[string]probe{
		"rebound host":       {path: "/assets", host: "rebind.evil.example:8787"},
		"rebound host write": {method: http.MethodPost, path: "/assets/" + assetID + "/rating", host: "rebind.evil.example:8787", body: `{"rating":1}`},
		"rebound session":    {method: http.MethodPost, path: "/auth/session", host: "rebind.evil.example"},
		"proxied":            {path: "/assets", headers: map[string]string{"X-Forwarded-For": "203.0.113.7"}},
		"proxied forwarded":  {path: "/assets", headers: map[string]string{"Forwarded": "for=203.0.113.7"}},
		"proxied public":     {path: "/assets", host: "photos.example.com", headers: map[string]string{"X-Forwarded-Host": "photos.example.com"}},
	} {
		if rec := p.do(srv); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status %d, want 401 (body %.120s)", name, rec.Code, rec.Body.String())
		}
	}
	for name, p := range map[string]probe{
		"localhost name": {path: "/assets", host: "localhost:8787"},
		"ipv6 loopback":  {path: "/assets", host: "[::1]:8787", remote: "[::1]:40000"},
		"token + foreign host": {path: "/assets", host: "photos.example.com",
			headers: map[string]string{"Authorization": "Bearer tok-123", "X-Forwarded-For": "203.0.113.7"}},
	} {
		if rec := p.do(srv); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200 (body %.120s)", name, rec.Code, rec.Body.String())
		}
	}
}

func TestOriginPolicy(t *testing.T) {
	pol, err := newOriginPolicy("https://hdr.toys/", "HTTPS://Library.Tail1234.ts.net:443", "https://*.preview.example", "", "http://dev.example:3000")
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/", nil)
	local.RemoteAddr = "127.0.0.1:1"
	remote := httptest.NewRequest(http.MethodGet, "https://library.tail1234.ts.net/", nil)
	remote.RemoteAddr = "100.64.0.9:1"

	for origin, want := range map[string]bool{
		"":                                 true,
		"https://hdr.toys":                 true,
		"https://HDR.toys:443":             true,
		"https://library.tail1234.ts.net":  true,
		"https://a.preview.example":        true,
		"https://a.b.preview.example":      true,
		"http://dev.example:3000":          true,
		"https://preview.example":          false,
		"http://a.preview.example":         false,
		"https://evilpreview.example":      false,
		"https://a.preview.example.evil":   false,
		"http://dev.example":               false,
		"http://hdr.toys":                  false,
		"https://hdr.toys.evil.example":    false,
		"https://user@hdr.toys":            false,
		"null":                             false,
		"file://":                          false,
		"chrome-extension://abcdef":        false,
		"https://":                         false,
		"http://localhost:3000":            false, // remote request below
		"http://127.0.0.1:3000":            false,
		"http://[::1]:3000":                false,
		"http://localhost.evil.example":    false,
		"http://127.0.0.1.evil.example:80": false,
	} {
		if got := pol.allows(remote, origin); got != want {
			t.Errorf("remote request, origin %q: allows=%v want %v", origin, got, want)
		}
	}
	for _, origin := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000", "https://app.localhost"} {
		if !pol.allows(local, origin) {
			t.Errorf("loopback request, origin %q: want allowed", origin)
		}
	}
	if pol.allows(local, "http://localhost.evil.example") || pol.allows(local, "null") {
		t.Errorf("loopback request must not allow look-alike or null origins")
	}

	for _, bad := range []string{"hdr.toys", "ftp://x", "https://*", "https://*.", "://", "https://a b"} {
		if _, err := newOriginPolicy(bad); err == nil {
			t.Errorf("newOriginPolicy(%q) accepted a malformed entry", bad)
		}
	}
	anyPol, err := newOriginPolicy("*")
	if err != nil || !anyPol.allows(remote, "https://evil.example") {
		t.Fatalf("explicit * must disable enforcement: %v", err)
	}
}

// AllowedOrigins is honoured end to end and a malformed entry fails startup.
func TestAllowedOriginsOption(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	opts := srv.opts
	opts.AllowedOrigins = []string{"https://preview.example"}
	extra, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if rec := (probe{path: "/assets", origin: "https://preview.example"}).do(extra); rec.Code != http.StatusOK {
		t.Fatalf("configured origin: status %d", rec.Code)
	}
	opts.AllowedOrigins = []string{"not an origin"}
	if _, err := New(opts); err == nil {
		t.Fatal("New accepted a malformed AllowedOrigins entry")
	}
}

// Tailnet identity is ambient too: a foreign page running on one of the
// owner's tailnet devices must be refused the same way.
func TestForeignOriginRefusedForTailnetIdentity(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	opts := srv.opts
	opts.PublicBaseURL = "https://library.tail1234.ts.net"
	opts.Auth = &auth.CompositeAuthenticator{
		Sessions: opts.Sessions,
		WhoIs: func(context.Context, string) (string, bool, error) {
			return "owner@example.com", true, nil
		},
	}
	tn, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	base := probe{method: http.MethodPost, path: "/auth/session", host: "library.tail1234.ts.net", remote: "100.64.0.9:51234"}

	evil := base
	evil.origin = "https://evil.example"
	if rec := evil.do(tn); rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "ses_") {
		t.Fatalf("foreign origin over tailnet: status %d body %.120s", rec.Code, rec.Body.String())
	}
	editor := base
	editor.origin = "https://hdr.toys"
	rec := editor.do(tn)
	var body struct {
		Token   string `json:"token"`
		Subject string `json:"subject"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || !strings.HasPrefix(body.Token, "ses_") || body.Subject != "tailnet:owner@example.com" {
		t.Fatalf("editor origin over tailnet: status %d body %.160s", rec.Code, rec.Body.String())
	}
}
