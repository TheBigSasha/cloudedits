package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A thumbnail revalidation is answered 304 for every If-None-Match form a
// browser or proxy may send: the exact tag, a list holding it, "*", and the
// weak form of the tag (If-None-Match uses the weak comparison).
func TestThumbnailIfNoneMatchForms(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	url := ts.URL + "/assets/" + assetID + "/file?tier=t1"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("thumbnail GET: status=%d etag=%q", resp.StatusCode, etag)
	}

	for _, tc := range []struct {
		header string
		want   int
	}{
		{etag, http.StatusNotModified},
		{`"other", ` + etag, http.StatusNotModified},
		{"*", http.StatusNotModified},
		{"W/" + etag, http.StatusNotModified},
		{`"other"`, http.StatusOK},
		{`W/"other"`, http.StatusOK},
	} {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("If-None-Match", tc.header)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != tc.want {
			t.Errorf("If-None-Match %q: status=%d, want %d", tc.header, r.StatusCode, tc.want)
		}
	}
}
