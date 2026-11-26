package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

const fuzzCanary = "CANARY-OUTSIDE-LIBRARY-7f3a9c"

// FuzzOpsBody posts arbitrary bodies to POST /ops as an authenticated owner.
// The server must never panic or answer 5xx, must always answer JSON, and must
// never leave a .edits sidecar on disk that its own parser rejects.
func FuzzOpsBody(f *testing.F) {
	srv, photoDir, assetID := newTestServer(f, "")
	editsPath := filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.edits")

	for _, s := range []string{
		`[{"hlc":"019a2f0012ab-0001-devA","device":"devA","assetKey":"` + assetID + `","field":"rating","value":5}]`,
		`{"ops":[{"hlc":"019a2f0012ab-0002-devA","device":"devA","assetKey":"` + assetID + `","field":"recipe","value":{"exposure":0.5,"nested":[1,2,{"a":null}]}}]}`,
		`{"ops":[{"hlc":"019a2f0012ab-0003-devA","device":"devA","assetKey":"` + assetID + `","field":"variant","value":{"id":"v1","name":"A","recipe":{"k":1}}}]}`,
		`{"ops":[{"hlc":"019a2f0012ab-0004-devA","device":"devA","assetKey":"` + assetID + `","field":"variant","value":{"id":"v1","tombstone":true}}]}`,
		`[{"hlc":"019a2f0012ab-0005-devA","device":"devA","file":"IMG_0001.JPG","field":"keywords","value":["a","b"]}]`,
		`[{"hlc":"019a2f0012ab-0006-devA","device":"devA","assetKey":"IMG_0001.JPG","field":"flag","value":"pick"}]`,
		`[{"hlc":"019a2f0012ab-0007-devA","device":"devA","field":"import","value":{}}]`,
		`[{"hlc":"ffffffffffffffff-ffffffff-z","device":"z","assetKey":"` + assetID + `","field":"rating","value":1e999}]`,
		`[{"hlc":"1-0-a","device":"a","assetKey":"` + assetID + `","field":"hdrt","value":"\u0000\ud800"}]`,
		`[{"hlc":"1-1-a","device":"a","assetKey":"../../etc/passwd","file":"..\\..\\x","field":"rating","value":1}]`,
		`{"ops":null}`, `{"ops":{}}`, `[]`, `[null]`, `[[]]`, `{}`, ``, ` `, `{"ops":[`, `"x"`, `[{"hlc":1}]`,
	} {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/ops", bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code >= 500 {
			t.Fatalf("POST /ops answered %d: %s", rec.Code, rec.Body.String())
		}
		if !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("POST /ops answered non-JSON (%d): %q", rec.Code, rec.Body.String())
		}
		if data, err := os.ReadFile(editsPath); err == nil {
			if _, perr := sidecar.Parse(data); perr != nil {
				t.Fatalf("server wrote a sidecar its own parser rejects: %v\n%s", perr, data)
			}
		}
		if entries, err := os.ReadDir(filepath.Join(photoDir, ".hdrtoys", ".quarantine")); err == nil && len(entries) > 0 {
			t.Fatalf("a POST /ops body caused a sidecar to be quarantined (%d entries)", len(entries))
		}
	})
}

// FuzzRequestPath sends arbitrary request targets through the full handler
// stack as an authenticated owner. No target may panic the server, produce a
// 5xx other than the documented 501 fallback, disclose bytes from outside the
// library, or redirect anywhere but the configured editor origin.
func FuzzRequestPath(f *testing.F) {
	srv, photoDir, assetID := newTestServer(f, "")
	if err := os.WriteFile(filepath.Join(filepath.Dir(photoDir), "canary.jpg"), []byte(fuzzCanary), 0o600); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photoDir, ".hidden-canary.jpg"), []byte(fuzzCanary), 0o600); err != nil {
		f.Fatal(err)
	}

	for _, s := range []string{
		"/", "/capabilities", "/open?a=x", "/h/abc", "/assets", "/assets/count", "/assets/range?offset=0&limit=5",
		"/assets/" + assetID, "/assets/" + assetID + "/file", "/assets/" + assetID + "/blob?tier=t1",
		"/assets/" + assetID + "/recipe", "/assets/" + assetID + "/variants", "/assets/" + assetID + "/renders",
		"/assets/" + assetID + "=/file", "/assets/" + strings.ToLower(assetID) + "/file",
		"/assets/..%2f..%2fcanary.jpg/file", "/assets/%2e%2e/%2e%2e/canary.jpg", "/assets/..\\..\\canary.jpg",
		"/assets/%00/file", "/assets/a%2Fb/file", "//assets//" + assetID + "//file", "/assets/./" + assetID + "/file",
		"/ASSETS/" + assetID + "/FILE", "/assets/.hidden-canary.jpg/file", "/assets/%252e%252e/x",
		"/assets?q=%25&sort=name%3BDROP&dir=asc&folder=%27&kind=..&rating=-1&keyword=%00&cursor=!!&limit=-5",
		"/changes?since=-1&limit=99999999999", "/handoff/..", "/__dev/stats", "/assets/%ff%fe/file", "*", "http://evil.example/assets",
	} {
		f.Add(uint8(0), s)
	}

	methods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, "PATCH", "CONNECT", "TRACE"}
	f.Fuzz(func(t *testing.T, methodIdx uint8, target string) {
		if strings.ContainsAny(target, " \r\n") || target == "" {
			t.Skip()
		}
		method := methods[int(methodIdx)%len(methods)]
		raw := method + " " + target + " HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 0\r\n\r\n"
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			t.Skip()
		}
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code >= 500 && rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s %q answered %d: %s", method, target, rec.Code, rec.Body.String())
		}
		if bytes.Contains(rec.Body.Bytes(), []byte(fuzzCanary)) {
			t.Fatalf("%s %q disclosed a file outside the indexed library", method, target)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			// Only two redirects exist: the editor hand-off and the router's
			// own same-origin path-cleaning redirect (a single leading slash).
			sameOrigin := strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") && !strings.HasPrefix(loc, "/\\")
			if !strings.HasPrefix(loc, "https://hdr.toys/") && !sameOrigin {
				t.Fatalf("%s %q redirected to %q", method, target, loc)
			}
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s %q response lacks X-Content-Type-Options: nosniff", method, target)
		}
	})
}
