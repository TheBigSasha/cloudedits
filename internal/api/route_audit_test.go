package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/muxaudit"
)

var pathWildcard = regexp.MustCompile(`\{[^}]*\}`)

// requestForPattern builds a concrete request matching a mux pattern such as
// "GET /assets/{id}/variants/{variantId}".
func requestForPattern(t *testing.T, pattern string) *http.Request {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		t.Fatalf("pattern %q has no method", pattern)
	}
	path = strings.ReplaceAll(path, "{$}", "")
	path = pathWildcard.ReplaceAllString(path, "probe")
	return httptest.NewRequest(method, "http://library.example"+path, strings.NewReader("{}"))
}

// TestEveryMuxRouteIsGovernedByRouteSpec enumerates the patterns actually
// registered on the live router (not the route table the server reports) and
// proves that each one is a declared RouteSpec whose auth policy and
// permission are enforced in front of the handler. A handler wired straight
// onto the mux, or a RouteSpec whose wrapper is bypassed, fails this test.
func TestEveryMuxRouteIsGovernedByRouteSpec(t *testing.T) {
	srv, _, _ := newTestServer(t, "")

	specs := map[string]RouteSpec{}
	for _, spec := range srv.Routes() {
		specs[spec.Method+" "+spec.Pattern] = spec
	}
	registered := muxaudit.Patterns(srv.mux)
	if len(registered) == 0 {
		t.Fatal("no patterns enumerated from the live mux; the audit cannot see the router")
	}

	want := []string{"/"}
	for key := range specs {
		want = append(want, key)
	}
	sort.Strings(want)
	if strings.Join(registered, "\n") != strings.Join(want, "\n") {
		t.Fatalf("live mux patterns differ from the RouteSpec table\nmux:\n  %s\nspecs (+ fallback):\n  %s",
			strings.Join(registered, "\n  "), strings.Join(want, "\n  "))
	}
	if extra := muxaudit.Patterns(http.DefaultServeMux); len(extra) != 0 {
		t.Fatalf("handlers registered on http.DefaultServeMux: %q", extra)
	}

	// A valid session whose principal holds no permission at all: every
	// Authenticated route must authenticate it and then refuse it.
	powerless, _ := srv.sessions.Issue(&auth.Principal{Subject: "audit:powerless", Scopes: map[auth.Permission]bool{}})

	for _, pattern := range registered {
		if pattern == "/" {
			continue
		}
		spec := specs[pattern]
		if spec.Auth == auth.AuthUnset || spec.ResourceScope < auth.ScopeNone || spec.SLOBudgetMs <= 0 {
			t.Errorf("%s: incomplete RouteSpec %+v", pattern, spec)
		}

		anon := requestForPattern(t, pattern)
		anon.RemoteAddr = "203.0.113.7:40000" // not loopback, no credentials
		anonRec := httptest.NewRecorder()
		srv.ServeHTTP(anonRec, anon)

		weak := requestForPattern(t, pattern)
		weak.RemoteAddr = "203.0.113.7:40000"
		weak.Header.Set("Authorization", "Bearer "+powerless)
		weakRec := httptest.NewRecorder()
		srv.ServeHTTP(weakRec, weak)

		switch spec.Auth {
		case auth.AuthAuthenticated:
			if spec.Permission == auth.PermNone {
				t.Errorf("%s: authenticated route declares no Permission", pattern)
			}
			if anonRec.Code != http.StatusUnauthorized {
				t.Errorf("%s: anonymous network request got %d, want 401", pattern, anonRec.Code)
			}
			if weakRec.Code != http.StatusForbidden {
				t.Errorf("%s: session lacking %q got %d, want 403", pattern, spec.Permission, weakRec.Code)
			}
		case auth.AuthDevOnly:
			if !HasTestRoutes() {
				t.Errorf("%s: dev-only route present in a release build", pattern)
			}
			if anonRec.Code != http.StatusForbidden || weakRec.Code != http.StatusForbidden {
				t.Errorf("%s: dev-only route answered %d/%d to a network caller, want 403", pattern, anonRec.Code, weakRec.Code)
			}
		case auth.AuthPublic:
			if spec.Permission != auth.PermNone {
				t.Errorf("%s: public route declares Permission %q it cannot enforce", pattern, spec.Permission)
			}
			if spec.Method != http.MethodGet && anonRec.Code != http.StatusUnauthorized {
				t.Errorf("%s: public non-GET route served an anonymous network caller (%d)", pattern, anonRec.Code)
			}
		default:
			t.Errorf("%s: unknown AuthPolicy %v", pattern, spec.Auth)
		}
	}

	// Anything outside the table lands on the fallback and is never served.
	for _, path := range []string{"/debug/pprof/", "/metrics", "/assets/x/unknown", "/__dev/reset-all", "/.hdrtoys/x.edits", "/api/state"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(method, "http://127.0.0.1"+path, nil)
			req.RemoteAddr = "127.0.0.1:40000"
			srv.ServeHTTP(rec, req)
			if rec.Code < 400 {
				t.Errorf("%s %s outside the route table answered %d", method, path, rec.Code)
			}
		}
	}
}
