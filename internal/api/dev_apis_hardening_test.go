//go:build dev_apis

package api

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// /__dev/reset deletes the sidecar directory. Loopback-only is not enough on
// its own: any web page can make the developer's browser POST to 127.0.0.1,
// and a rebound hostname or local proxy also arrives from loopback.
func TestDevResetRefusesCrossSiteReboundAndProxiedCallers(t *testing.T) {
	scratch := t.TempDir()
	srv, _, _ := newTestServer(t, scratch)
	marker := filepath.Join(scratch, "main", "marker.edits")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, p := range map[string]probe{
		"cross-site form post": {method: http.MethodPost, path: "/__dev/reset", origin: "https://evil.example", ctype: "text/plain"},
		"DNS rebinding":        {method: http.MethodPost, path: "/__dev/reset", host: "rebind.evil.example:8787"},
		"local reverse proxy":  {method: http.MethodPost, path: "/__dev/reset", headers: map[string]string{"X-Forwarded-For": "203.0.113.7"}},
		"network peer":         {method: http.MethodPost, path: "/__dev/reset", remote: "192.168.1.50:40000"},
		"cross-site fault":     {method: http.MethodPost, path: "/__dev/fault", origin: "https://evil.example", ctype: "text/plain", body: `{"offline":true}`},
	} {
		if rec := p.do(srv); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, rec.Code)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a refused /__dev/reset deleted sidecar data: %v", err)
	}
	if rec := (probe{path: "/capabilities"}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("a refused /__dev/fault took the server offline: %d", rec.Code)
	}

	// The local test harness (no Origin, direct loopback) still works.
	if rec := (probe{method: http.MethodPost, path: "/__dev/reset"}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("local harness reset: status %d body %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("local harness reset did not clear the scratch sidecar dir (err=%v)", err)
	}
}
