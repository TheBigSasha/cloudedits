package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func getJSON(t *testing.T, srv *Server, target string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:50000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestRepeatedKindParameterIsOR pins that every kind=<v> occurrence reaches
// the index filter, not only the first one.
func TestRepeatedKindParameterIsOR(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	for _, target := range []string{
		"/assets/count?kind=raw&kind=image",
		"/assets/count?kind=image&kind=raw",
		"/assets/count?kind=raw&kind=jpeg",
	} {
		code, body := getJSON(t, srv, target)
		if code != http.StatusOK || body["total"] != float64(1) {
			t.Errorf("%s: code=%d body=%v, want total=1", target, code, body)
		}
	}
}
