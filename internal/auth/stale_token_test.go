package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Sessions are held in memory, so every token an open tab holds dies with a
// server restart. A stale token must not lock out a caller whose connection
// identity alone would be accepted, and must never grant anything on its own.
func TestStaleTokenFallsBackToConnectionIdentity(t *testing.T) {
	beforeRestart := NewSessionManager(time.Hour)
	staleSession, _ := beforeRestart.Issue(OwnerPrincipal("tailnet:alice@example.com"))

	whoIs := func(_ context.Context, addr string) (string, bool, error) {
		if addr == "100.64.0.9:51234" {
			return "alice@example.com", true, nil
		}
		return "stranger@other.example", false, nil
	}
	afterRestart := &CompositeAuthenticator{Sessions: NewSessionManager(time.Hour), StaticToken: "secret-123", WhoIs: whoIs, AllowLoopback: true}

	req := func(remote, host, bearer string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+"/assets/a1/file", nil)
		r.RemoteAddr = remote
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		return r
	}

	for name, tc := range map[string]struct {
		r    *http.Request
		want string
	}{
		"tailnet owner, stale session":  {req("100.64.0.9:51234", "library.example.ts.net", staleSession), "tailnet:alice@example.com"},
		"tailnet owner, garbage bearer": {req("100.64.0.9:51234", "library.example.ts.net", "nonsense"), "tailnet:alice@example.com"},
		"local owner, stale session":    {req("127.0.0.1:5000", "127.0.0.1:8787", staleSession), "loopback:local"},
	} {
		p, err := afterRestart.Authenticate(tc.r)
		if err != nil || p.Subject != tc.want {
			t.Errorf("%s: got %+v, %v; want subject %q", name, p, err, tc.want)
		}
	}

	for name, r := range map[string]*http.Request{
		"other tailnet user, stale session": req("100.64.0.77:51234", "library.example.ts.net", staleSession),
		"network peer, stale session":       req("192.168.1.50:5000", "192.168.1.10:8787", staleSession),
		"rebound host, stale session":       req("127.0.0.1:5000", "rebind.evil.example", staleSession),
	} {
		if p, err := afterRestart.Authenticate(r); err == nil {
			t.Errorf("%s: stale token was accepted as %q", name, p.Subject)
		}
	}

	// Token mode: connection identity never applies, stale or not.
	tokenMode := &CompositeAuthenticator{Sessions: NewSessionManager(time.Hour), StaticToken: "secret-123", RequireToken: true, WhoIs: whoIs, AllowLoopback: true}
	for name, r := range map[string]*http.Request{
		"tailnet owner, stale session": req("100.64.0.9:51234", "library.example.ts.net", staleSession),
		"local owner, wrong token":     req("127.0.0.1:5000", "127.0.0.1:8787", "secret-124"),
		"local owner, no token":        req("127.0.0.1:5000", "127.0.0.1:8787", ""),
	} {
		if p, err := tokenMode.Authenticate(r); err == nil {
			t.Errorf("token mode, %s: accepted as %q", name, p.Subject)
		}
	}
	if p, err := tokenMode.Authenticate(req("203.0.113.7:5000", "photos.example.com", "secret-123")); err != nil || p.Subject != "token:default" {
		t.Errorf("token mode, correct token: got %+v, %v", p, err)
	}
}
