package api

import (
	"bytes"
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// openAPIDoc is the slice of docs/openapi.yaml these tests hold the server
// to: every operation with its documented response codes, and the required
// property lists of the component schemas.
type openAPIDoc struct {
	ops      map[string]map[string]bool // "GET /assets/{id}" -> documented status codes
	required map[string][]string        // schema name -> required properties
}

var (
	reYAMLPath     = regexp.MustCompile(`^  (/[^:]*):\s*$`)
	reYAMLMethod   = regexp.MustCompile(`^    (get|post|put|delete):\s*$`)
	reYAMLStatus   = regexp.MustCompile(`^        "(\d{3})":\s*$`)
	reYAMLSchema   = regexp.MustCompile(`^    ([A-Za-z]+):\s*$`)
	reYAMLRequired = regexp.MustCompile(`^      required: \[(.*)\]\s*$`)
)

// loadOpenAPI reads the document with a line scanner keyed to its fixed
// two-space layout, so the contract tests need no YAML dependency.
func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	doc := openAPIDoc{ops: map[string]map[string]bool{}, required: map[string][]string{}}
	var section, path, op, schema string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case line == "paths:":
			section = "paths"
		case line == "components:":
			section = "components"
		case section == "paths":
			if m := reYAMLPath.FindStringSubmatch(line); m != nil {
				path = m[1]
			} else if m := reYAMLMethod.FindStringSubmatch(line); m != nil {
				op = strings.ToUpper(m[1]) + " " + path
				doc.ops[op] = map[string]bool{}
			} else if m := reYAMLStatus.FindStringSubmatch(line); m != nil && op != "" {
				doc.ops[op][m[1]] = true
			}
		case section == "components":
			if m := reYAMLSchema.FindStringSubmatch(line); m != nil {
				schema = m[1]
			} else if m := reYAMLRequired.FindStringSubmatch(line); m != nil && schema != "" {
				for _, f := range strings.Split(m[1], ",") {
					doc.required[schema] = append(doc.required[schema], strings.TrimSpace(f))
				}
			}
		}
	}
	if len(doc.ops) == 0 || len(doc.required["AssetSummary"]) == 0 {
		t.Fatalf("could not read docs/openapi.yaml (ops=%d)", len(doc.ops))
	}
	return doc
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestOpenAPIDocumentsExactlyTheRouteTable makes docs/openapi.yaml the single
// description of the HTTP surface: a route cannot be added, removed or renamed
// without the document changing with it.
func TestOpenAPIDocumentsExactlyTheRouteTable(t *testing.T) {
	doc := loadOpenAPI(t)
	srv, _, _ := newTestServer(t, "")
	routes := map[string]bool{}
	for _, r := range srv.coreRoutes() {
		routes[r.Method+" "+strings.Replace(r.Pattern, "/{$}", "/", 1)] = true
	}
	for _, key := range sortedKeys(routes) {
		if doc.ops[key] == nil {
			t.Errorf("route %q is served but not documented in docs/openapi.yaml", key)
		}
	}
	for _, key := range sortedKeys(doc.ops) {
		if !routes[key] {
			t.Errorf("docs/openapi.yaml documents %q, which the server does not route", key)
		}
	}
}

// TestLiveResponsesMatchOpenAPI replays every documented GET operation
// against a live server and checks the status code is one the document lists
// and that JSON bodies carry the documented required properties.
func TestLiveResponsesMatchOpenAPI(t *testing.T) {
	doc := loadOpenAPI(t)
	srv, _, assetID := newTestServer(t, "")

	do := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1"+target, nil)
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	for _, key := range sortedKeys(doc.ops) {
		method, path, _ := strings.Cut(key, " ")
		if method != http.MethodGet {
			continue
		}
		target := strings.NewReplacer("{id}", assetID, "{variantId}", "no-such-variant", "{channel}", "depth").Replace(path)
		rec := do(method, target)
		if code := rec.Result().Status[:3]; !doc.ops[key][code] {
			t.Errorf("%s -> %s, documented codes are %v", key, code, sortedKeys(doc.ops[key]))
		}
	}

	requireProps := func(what string, body []byte, schema string) {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("%s: not a JSON object: %v", what, err)
		}
		if len(doc.required[schema]) == 0 {
			t.Errorf("docs/openapi.yaml has no schema %s with required properties", schema)
		}
		for _, prop := range doc.required[schema] {
			if _, ok := obj[prop]; !ok {
				t.Errorf("%s lacks required %s.%s", what, schema, prop)
			}
		}
	}
	requireProps("GET /capabilities", do(http.MethodGet, "/capabilities").Body.Bytes(), "CapabilitiesResponse")
	requireProps("GET /assets/{id}", do(http.MethodGet, "/assets/"+assetID).Body.Bytes(), "AssetSummary")
	requireProps("GET /changes", do(http.MethodGet, "/changes").Body.Bytes(), "ChangesResponse")
	requireProps("GET /assets/missing (error)", do(http.MethodGet, "/assets/bm9wZQ").Body.Bytes(), "ErrorResponse")

	var page struct {
		Assets []json.RawMessage `json:"assets"`
	}
	if err := json.Unmarshal(do(http.MethodGet, "/assets/range?fields=grid&limit=1").Body.Bytes(), &page); err != nil || len(page.Assets) != 1 {
		t.Fatalf("grid page: %v (%d assets)", err, len(page.Assets))
	}
	requireProps("GET /assets/range?fields=grid item", page.Assets[0], "GridAsset")

	for _, tc := range []struct {
		label string
		body  []byte
		want  string
	}{
		{"JPEG", sampleJPEG(t, 4, 4, color.RGBA{1, 2, 3, 255}), "image/jpeg"},
		{"PNG", samplePNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), "image/png"},
	} {
		putAuxReq := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/assets/"+assetID+"/aux/depth", bytes.NewReader(tc.body))
		putAuxReq.Header.Set("Content-Type", tc.want)
		putAuxReq.RemoteAddr = "127.0.0.1:50000"
		putAuxRec := httptest.NewRecorder()
		srv.ServeHTTP(putAuxRec, putAuxReq)
		if putAuxRec.Code != http.StatusOK {
			t.Fatalf("PUT /assets/{id}/aux/depth (%s): status=%d body=%s", tc.label, putAuxRec.Code, putAuxRec.Body.String())
		}
		requireProps("PUT /assets/{id}/aux/depth ("+tc.label+")", putAuxRec.Body.Bytes(), "AuxChannelUploadResponse")
		getAuxRec := do(http.MethodGet, "/assets/"+assetID+"/aux/depth")
		if getAuxRec.Code != http.StatusOK || getAuxRec.Header().Get("Content-Type") != tc.want {
			t.Fatalf("GET /assets/{id}/aux/depth (%s): status=%d ct=%q want %q", tc.label, getAuxRec.Code, getAuxRec.Header().Get("Content-Type"), tc.want)
		}
	}
	requireProps("DELETE /assets/{id}/aux/depth", do(http.MethodDelete, "/assets/"+assetID+"/aux/depth").Body.Bytes(), "AuxChannelDeleteResponse")
}
