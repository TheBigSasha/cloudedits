package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSessionAndBlobTokens(t *testing.T) {
	sm := NewSessionManager(5 * time.Minute)
	p := OwnerPrincipal("tailnet:alice@example.com")

	tok, exp := sm.Issue(p)
	if tok == "" || !exp.After(time.Now()) {
		t.Fatalf("invalid issued session token")
	}
	got, ok := sm.Validate(tok)
	if !ok || got.Subject != p.Subject {
		t.Fatalf("expected valid session for %s", p.Subject)
	}

	bt := sm.SignBlobToken(p.Subject, "asset-123", 5*time.Minute)
	bp, ok := sm.VerifyBlobToken(bt, "asset-123")
	if !ok || !bp.HasPermission(PermAssetRead) || bp.HasPermission(PermEditsWrite) {
		t.Fatalf("blob token must grant only asset:read for asset-123")
	}
	if _, ok := sm.VerifyBlobToken(bt, "asset-other"); ok {
		t.Fatalf("blob token must reject mismatched asset ID")
	}
}

func TestCompositeAuthenticatorModes(t *testing.T) {
	sm := NewSessionManager(5 * time.Minute)
	a := &CompositeAuthenticator{
		Sessions:      sm,
		StaticToken:   "secret-123",
		RequireToken:  false,
		AllowLoopback: true,
		WhoIs: func(_ context.Context, addr string) (string, bool, error) {
			if addr == "100.64.0.9:51234" {
				return "alice@example.com", true, nil
			}
			return "stranger@other.com", false, nil
		},
	}

	// 1. Tailnet owner succeeds.
	r := httptest.NewRequest(http.MethodGet, "/assets", nil)
	r.RemoteAddr = "100.64.0.9:51234"
	p, err := a.Authenticate(r)
	if err != nil || p.Subject != "tailnet:alice@example.com" {
		t.Fatalf("expected tailnet owner, got p=%+v err=%v", p, err)
	}

	// 2. Non-owner tailnet peer rejected.
	r2 := httptest.NewRequest(http.MethodGet, "/assets", nil)
	r2.RemoteAddr = "100.64.0.99:51234"
	if _, err := a.Authenticate(r2); err == nil {
		t.Fatalf("expected non-owner tailnet peer to be rejected")
	}

	// 3. Static bearer token succeeds even when RequireToken=true.
	a.RequireToken = true
	r3 := httptest.NewRequest(http.MethodGet, "/assets", nil)
	r3.RemoteAddr = "127.0.0.1:4444"
	if _, err := a.Authenticate(r3); err == nil {
		t.Fatalf("expected loopback without token to fail when RequireToken=true")
	}
	r3.Header.Set("Authorization", "Bearer secret-123")
	if p, err := a.Authenticate(r3); err != nil || p.Subject != "token:default" {
		t.Fatalf("expected bearer token auth, got p=%+v err=%v", p, err)
	}
}
