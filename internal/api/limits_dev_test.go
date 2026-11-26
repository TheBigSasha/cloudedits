//go:build dev_apis

package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestDevFaultBodyIsBounded(t *testing.T) {
	srv, _, _ := newTestServer(t, t.TempDir())
	huge := `{"pathPrefix":"` + strings.Repeat("A", 4<<20) + `"}`
	if rec := (probe{method: http.MethodPost, path: "/__dev/fault", body: huge}).do(srv); rec.Code != http.StatusBadRequest {
		t.Fatalf("4 MiB fault body: status %d, want 400", rec.Code)
	}
}
