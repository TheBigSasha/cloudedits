package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1":              true,
		"127.0.0.1:8787":         true,
		"127.8.9.10:1":           true,
		"localhost":              true,
		"LOCALHOST:8787":         true,
		"localhost.":             true,
		"app.localhost:3000":     true,
		"[::1]":                  true,
		"[::1]:8787":             true,
		"::1":                    true,
		"":                       false,
		"0.0.0.0:8787":           false,
		"192.168.1.10:8787":      false,
		"100.64.0.9":             false,
		"library.tail1.ts.net":   false,
		"localhost.evil.example": false,
		"127.0.0.1.evil.example": false,
		"evil.example:8787":      false,
		"notlocalhost":           false,
		"[fe80::1]:8787":         false,
	} {
		if got := IsLoopbackHost(host); got != want {
			t.Errorf("IsLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// Loopback trust is ambient: it must hold only for a direct connection from
// this machine addressed to this machine.
func TestLoopbackTrustDefeatsRebindingAndProxies(t *testing.T) {
	a := &CompositeAuthenticator{AllowLoopback: true, StaticToken: "secret-123"}
	req := func(remote, host string, hdr ...string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder/assets", nil)
		r.RemoteAddr, r.Host = remote, host
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return r
	}

	for name, r := range map[string]*http.Request{
		"direct ipv4":      req("127.0.0.1:5000", "127.0.0.1:8787"),
		"direct localhost": req("127.0.0.1:5000", "localhost:8787"),
		"direct ipv6":      req("[::1]:5000", "[::1]:8787"),
	} {
		if p, err := a.Authenticate(r); err != nil || p.Subject != "loopback:local" {
			t.Errorf("%s: want loopback owner, got %+v %v", name, p, err)
		}
	}
	for name, r := range map[string]*http.Request{
		"DNS rebinding":       req("127.0.0.1:5000", "attacker.example:8787"),
		"rebinding lookalike": req("127.0.0.1:5000", "localhost.attacker.example"),
		"empty host":          req("127.0.0.1:5000", ""),
		"reverse proxy xff":   req("127.0.0.1:5000", "127.0.0.1:8787", "X-Forwarded-For", "203.0.113.7"),
		"reverse proxy fwd":   req("127.0.0.1:5000", "127.0.0.1:8787", "Forwarded", "for=203.0.113.7"),
		"reverse proxy host":  req("127.0.0.1:5000", "localhost", "X-Forwarded-Host", "photos.example.com"),
		"reverse proxy real":  req("127.0.0.1:5000", "localhost", "X-Real-IP", "203.0.113.7"),
		"network peer":        req("192.168.1.50:5000", "127.0.0.1:8787"),
	} {
		if p, err := a.Authenticate(r); err == nil {
			t.Errorf("%s: granted %q, want unauthorized", name, p.Subject)
		}
	}
	// An explicit credential is not ambient and is unaffected.
	r := req("127.0.0.1:5000", "photos.example.com", "X-Forwarded-For", "203.0.113.7", "Authorization", "Bearer secret-123")
	if p, err := a.Authenticate(r); err != nil || p.Subject != "token:default" {
		t.Errorf("proxied bearer token: got %+v %v", p, err)
	}
}
