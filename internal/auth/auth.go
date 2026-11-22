// Package auth implements the declarative authentication, permission, and
// scope primitives for cloudedits, plus short-lived session and ?bt= blob tokens.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AuthPolicy declares whether a route is public, authenticated, or dev-only.
type AuthPolicy int

const (
	// AuthUnset causes fail-closed router validation failure.
	AuthUnset AuthPolicy = iota
	// AuthPublic allows unauthenticated requests (e.g. /capabilities, redirects).
	AuthPublic
	// AuthAuthenticated requires a verified Principal and an explicit Permission.
	AuthAuthenticated
	// AuthDevOnly is strictly permitted only in //go:build dev_apis builds.
	AuthDevOnly
)

func (p AuthPolicy) String() string {
	switch p {
	case AuthPublic:
		return "Public"
	case AuthAuthenticated:
		return "Authenticated"
	case AuthDevOnly:
		return "DevOnly"
	default:
		return "Unset"
	}
}

// Permission enumerates fine-grained capabilities matching GET /capabilities.
type Permission string

const (
	PermNone           Permission = ""
	PermLibraryRead    Permission = "library:read"
	PermAssetRead      Permission = "asset:read"
	PermEditsWrite     Permission = "edits:write"
	PermMediaWrite     Permission = "media:write"
	PermThumbnailWrite Permission = "thumbnail:write"
	PermVariantsWrite  Permission = "variants:write"
	PermHandoffCreate  Permission = "handoff:create"
	PermAdminManage    Permission = "admin:manage"
)

// OwnerPermissions lists all permissions granted to the library owner.
func OwnerPermissions() []Permission {
	return []Permission{
		PermLibraryRead,
		PermAssetRead,
		PermEditsWrite,
		PermMediaWrite,
		PermThumbnailWrite,
		PermVariantsWrite,
		PermHandoffCreate,
		PermAdminManage,
	}
}

// ResourceScope declares the authorization boundary of an endpoint.
type ResourceScope int

const (
	ScopeNone ResourceScope = iota
	ScopeLibrary
	ScopeFolder
	ScopeAsset
)

// Role represents a per-library RBAC grant (extension point for multi-user OIDC).
type Role string

const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
	RoleAdmin  Role = "admin"
)

// ResourceRef identifies the resource targeted by an HTTP request.
type ResourceRef struct {
	LibraryID string
	FolderID  string
	AssetID   string
}

// Principal is the authenticated caller attached to a request.
type Principal struct {
	Subject   string // "tailnet:<user>" | "token:default" | "loopback:local" | "oidc:<sub>"
	Scopes    map[Permission]bool
	Libraries map[string]Role // nil means owner/all libraries
}

// HasPermission reports whether p holds perm.
func (p *Principal) HasPermission(perm Permission) bool {
	if p == nil {
		return false
	}
	if perm == PermNone {
		return true
	}
	if p.Scopes[perm] {
		return true
	}
	// Align media:write and thumbnail:write cleanly across client versions.
	if perm == PermThumbnailWrite && p.Scopes[PermMediaWrite] {
		return true
	}
	if perm == PermMediaWrite && p.Scopes[PermThumbnailWrite] {
		return true
	}
	return false
}

// OwnerPrincipal creates a full-permission owner principal.
func OwnerPrincipal(subject string) *Principal {
	scopes := make(map[Permission]bool, len(OwnerPermissions()))
	for _, perm := range OwnerPermissions() {
		scopes[perm] = true
	}
	return &Principal{
		Subject:   subject,
		Scopes:    scopes,
		Libraries: nil,
	}
}

// Authenticator extracts and validates a caller's Principal.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// Authorizer enforces Permission and ResourceScope checks on a Principal.
type Authorizer interface {
	Authorize(p *Principal, perm Permission, scope ResourceScope, res ResourceRef) error
}

// ErrUnauthorized indicates missing or invalid credentials.
var ErrUnauthorized = errors.New("unauthorized")

// ErrForbidden indicates valid credentials lacking required permission/scope.
var ErrForbidden = errors.New("forbidden")

// DefaultAuthorizer checks Permission membership and optional per-library Role.
type DefaultAuthorizer struct{}

// Authorize verifies p has perm for res.
func (DefaultAuthorizer) Authorize(p *Principal, perm Permission, _ ResourceScope, res ResourceRef) error {
	if p == nil {
		return ErrUnauthorized
	}
	if !p.HasPermission(perm) {
		return fmt.Errorf("%w: missing %s", ErrForbidden, perm)
	}
	if p.Libraries != nil && res.LibraryID != "" {
		if _, ok := p.Libraries[res.LibraryID]; !ok {
			return fmt.Errorf("%w: no grant for library %s", ErrForbidden, res.LibraryID)
		}
	}
	return nil
}

// WhoIsFunc resolves a remote address to a verified Tailnet peer login name
// and whether the peer belongs to the owner account.
type WhoIsFunc func(ctx context.Context, remoteAddr string) (login string, isOwner bool, err error)

// SessionManager issues short-lived tab-scoped session tokens and HMAC-signed
// ?bt= blob tokens (so <img> requests never need long-lived secrets in URLs).
type SessionManager struct {
	mu       sync.RWMutex
	secret   []byte
	sessions map[string]sessionRecord
	ttl      time.Duration
}

type sessionRecord struct {
	principal *Principal
	expiresAt time.Time
}

// NewSessionManager creates a new SessionManager with a fresh random HMAC key.
func NewSessionManager(ttl time.Duration) *SessionManager {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return &SessionManager{
		secret:   secret,
		sessions: map[string]sessionRecord{},
		ttl:      ttl,
	}
}

// Issue creates a short-lived session bearer token for p.
func (sm *SessionManager) Issue(p *Principal) (token string, expiresAt time.Time) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token = "ses_" + hex.EncodeToString(b)
	expiresAt = time.Now().UTC().Add(sm.ttl)

	sm.mu.Lock()
	sm.sessions[token] = sessionRecord{principal: p, expiresAt: expiresAt}
	sm.mu.Unlock()
	return token, expiresAt
}

// Validate checks a session bearer token and returns its Principal.
func (sm *SessionManager) Validate(token string) (*Principal, bool) {
	sm.mu.RLock()
	rec, ok := sm.sessions[token]
	sm.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().UTC().After(rec.expiresAt) {
		sm.mu.Lock()
		delete(sm.sessions, token)
		sm.mu.Unlock()
		return nil, false
	}
	return rec.principal, true
}

// SignBlobToken generates a short-lived HMAC-signed ?bt= token scoped to assetID
// (or "*" for library-wide session blob reads) and subject.
func (sm *SessionManager) SignBlobToken(subject, assetID string, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	exp := time.Now().UTC().Add(ttl).Unix()
	payload := fmt.Sprintf("%s|%s|%d", subject, assetID, exp)
	mac := hmac.New(sha256.New, sm.secret)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
}

// VerifyBlobToken validates a ?bt= signed token for assetID and returns a
// read-only Principal scoped to asset:read.
func (sm *SessionManager) VerifyBlobToken(bt, assetID string) (*Principal, bool) {
	parts := strings.Split(bt, ".")
	if len(parts) != 2 {
		return nil, false
	}
	rawPayload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, sm.secret)
	mac.Write(rawPayload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	fields := strings.Split(string(rawPayload), "|")
	if len(fields) != 3 {
		return nil, false
	}
	subject, scopedAsset, expStr := fields[0], fields[1], fields[2]
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().UTC().Unix() > exp {
		return nil, false
	}
	if scopedAsset != "*" && scopedAsset != assetID {
		return nil, false
	}
	return &Principal{
		Subject: subject,
		Scopes: map[Permission]bool{
			PermAssetRead: true,
		},
	}, true
}

// CompositeAuthenticator combines session tokens, static bearer tokens,
// Tailnet WhoIs identity, ?bt= blob tokens, and optional loopback access.
type CompositeAuthenticator struct {
	Sessions      *SessionManager
	StaticToken   string
	RequireToken  bool
	AllowLoopback bool
	WhoIs         WhoIsFunc
}

// Authenticate resolves the caller Principal or returns ErrUnauthorized.
func (a *CompositeAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	// 1. Bearer header: check issued session tokens, then static token.
	if tok := ExtractBearer(r); tok != "" {
		if a.Sessions != nil {
			if p, ok := a.Sessions.Validate(tok); ok {
				return p, nil
			}
		}
		if a.StaticToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.StaticToken)) == 1 {
			return OwnerPrincipal("token:default"), nil
		}
		return nil, ErrUnauthorized
	}

	// 2. Signed ?bt= query parameter (scoped solely to asset blob reads).
	if bt := r.URL.Query().Get("bt"); bt != "" && a.Sessions != nil {
		assetID := r.PathValue("id")
		if p, ok := a.Sessions.VerifyBlobToken(bt, assetID); ok {
			return p, nil
		}
		return nil, ErrUnauthorized
	}

	if a.RequireToken {
		return nil, ErrUnauthorized
	}

	// 3. Verified Tailscale WhoIs identity.
	if a.WhoIs != nil {
		login, isOwner, err := a.WhoIs(r.Context(), r.RemoteAddr)
		if err == nil && login != "" && isOwner {
			return OwnerPrincipal("tailnet:" + login), nil
		}
	}

	// 4. Explicit loopback mode (local dev / CLI / single-host setup).
	if a.AllowLoopback && IsLoopbackRemoteAddr(r.RemoteAddr) {
		return OwnerPrincipal("loopback:local"), nil
	}

	return nil, ErrUnauthorized
}

// ExtractBearer extracts the token from an Authorization: Bearer <tok> header.
func ExtractBearer(r *http.Request) string {
	hdr := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(hdr) < 7 || !strings.EqualFold(hdr[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(hdr[7:])
}

// IsLoopbackRemoteAddr reports whether remoteAddr is a loopback IP.
func IsLoopbackRemoteAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
