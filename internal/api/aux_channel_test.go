package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/auth"
)

func samplePNG(t testing.TB, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAuxChannelCapabilitiesAdvertised(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
	code, body := getJSON(t, srv, "/capabilities")
	if code != http.StatusOK {
		t.Fatalf("GET /capabilities status=%d", code)
	}
	caps, _ := body["caps"].(map[string]any)
	if caps["auxiliaryChannels"] != true {
		t.Fatalf("caps.auxiliaryChannels = %v, want true", caps["auxiliaryChannels"])
	}
	limits, _ := body["limits"].(map[string]any)
	if limits["maxAuxChannelBytes"] != float64(MaxAuxChannelBytes) {
		t.Fatalf("limits.maxAuxChannelBytes = %v, want %d", limits["maxAuxChannelBytes"], MaxAuxChannelBytes)
	}
}

func TestAuxChannelFullLifecycle(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	origBytes, err := os.ReadFile(filepath.Join(photoDir, "IMG_0001.JPG"))
	if err != nil {
		t.Fatal(err)
	}

	auxURL := ts.URL + "/assets/" + assetID + "/aux/depth"

	// 1. GET before upload returns 404.
	resp, err := http.Get(auxURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET before PUT status=%d, want 404", resp.StatusCode)
	}

	// 2. PUT valid PNG persists atomically under .hdrtoys/IMG_0001.JPG.depth.png.
	depthV1 := samplePNG(t, 16, 16, color.RGBA{120, 120, 120, 255})
	putReq, _ := http.NewRequest(http.MethodPut, auxURL, bytes.NewReader(depthV1))
	putReq.Header.Set("Content-Type", "image/png")
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	var putBody struct {
		OK          bool   `json:"ok"`
		AssetID     string `json:"assetId"`
		Channel     string `json:"channel"`
		Bytes       int    `json:"bytes"`
		ContentType string `json:"contentType"`
		ETag        string `json:"etag"`
	}
	if err := json.NewDecoder(putResp.Body).Decode(&putBody); err != nil {
		t.Fatal(err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status=%d, want 200", putResp.StatusCode)
	}
	if !putBody.OK || putBody.AssetID != assetID || putBody.Channel != "depth" || putBody.Bytes != len(depthV1) || putBody.ContentType != "image/png" || putBody.ETag == "" {
		t.Fatalf("unexpected PUT response body: %+v", putBody)
	}
	if !strings.HasPrefix(putBody.ETag, `"`) || !strings.HasSuffix(putBody.ETag, `"`) {
		t.Fatalf("expected strong quoted ETag, got %q", putBody.ETag)
	}
	diskPath := filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.depth.png")
	diskBytes, err := os.ReadFile(diskPath)
	if err != nil || !bytes.Equal(diskBytes, depthV1) {
		t.Fatalf("sidecar file mismatch on disk (err=%v)", err)
	}

	// 3. GET returns raw PNG bytes, strong ETag, Cache-Control, and X-HDRT-Aux-Channel.
	getResp, err := http.Get(auxURL)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := io.ReadAll(getResp.Body)
	_ = getResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d, want 200", getResp.StatusCode)
	}
	if getResp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("Content-Type=%q, want image/png", getResp.Header.Get("Content-Type"))
	}
	if getResp.Header.Get("ETag") != putBody.ETag {
		t.Fatalf("GET ETag=%q, want %q", getResp.Header.Get("ETag"), putBody.ETag)
	}
	if getResp.Header.Get("Cache-Control") != "private, max-age=3600" {
		t.Fatalf("Cache-Control=%q, want private, max-age=3600", getResp.Header.Get("Cache-Control"))
	}
	if getResp.Header.Get("X-HDRT-Aux-Channel") != "depth" {
		t.Fatalf("X-HDRT-Aux-Channel=%q, want depth", getResp.Header.Get("X-HDRT-Aux-Channel"))
	}
	if !bytes.Equal(gotBytes, depthV1) {
		t.Fatalf("GET body mismatch: got %d bytes, want %d", len(gotBytes), len(depthV1))
	}

	// 4. Conditional GET with matching If-None-Match returns 304 Not Modified.
	inmReq, _ := http.NewRequest(http.MethodGet, auxURL, nil)
	inmReq.Header.Set("If-None-Match", putBody.ETag)
	inmResp, err := http.DefaultClient.Do(inmReq)
	if err != nil {
		t.Fatal(err)
	}
	inmBody, _ := io.ReadAll(inmResp.Body)
	_ = inmResp.Body.Close()
	if inmResp.StatusCode != http.StatusNotModified || len(inmBody) != 0 {
		t.Fatalf("If-None-Match match: status=%d len=%d, want 304 empty", inmResp.StatusCode, len(inmBody))
	}
	if inmResp.Header.Get("ETag") != putBody.ETag || inmResp.Header.Get("X-HDRT-Aux-Channel") != "depth" {
		t.Fatalf("304 headers missing ETag or X-HDRT-Aux-Channel: %v", inmResp.Header)
	}

	// Mismatched If-None-Match returns 200.
	missReq, _ := http.NewRequest(http.MethodGet, auxURL, nil)
	missReq.Header.Set("If-None-Match", `"stale-etag"`)
	missResp, err := http.DefaultClient.Do(missReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = missResp.Body.Close()
	if missResp.StatusCode != http.StatusOK {
		t.Fatalf("If-None-Match mismatch: status=%d, want 200", missResp.StatusCode)
	}

	// 5. Overwriting changes ETag and bytes cleanly.
	depthV2 := samplePNG(t, 16, 16, color.RGBA{240, 240, 240, 255})
	putReq2, _ := http.NewRequest(http.MethodPut, auxURL, bytes.NewReader(depthV2))
	putResp2, err := http.DefaultClient.Do(putReq2)
	if err != nil {
		t.Fatal(err)
	}
	var putBody2 struct {
		ETag string `json:"etag"`
	}
	_ = json.NewDecoder(putResp2.Body).Decode(&putBody2)
	_ = putResp2.Body.Close()
	if putResp2.StatusCode != http.StatusOK || putBody2.ETag == putBody.ETag {
		t.Fatalf("overwrite PUT status=%d etag=%q (prev=%q)", putResp2.StatusCode, putBody2.ETag, putBody.ETag)
	}

	// 6. DELETE removes disposable sidecar only, never touching original photo.
	delReq, _ := http.NewRequest(http.MethodDelete, auxURL, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	var delBody struct {
		OK      bool   `json:"ok"`
		AssetID string `json:"assetId"`
		Channel string `json:"channel"`
		Deleted bool   `json:"deleted"`
	}
	_ = json.NewDecoder(delResp.Body).Decode(&delBody)
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK || !delBody.OK || !delBody.Deleted || delBody.AssetID != assetID || delBody.Channel != "depth" {
		t.Fatalf("DELETE first: status=%d body=%+v", delResp.StatusCode, delBody)
	}
	if _, err := os.Stat(diskPath); !os.IsNotExist(err) {
		t.Fatalf("expected sidecar removed after DELETE, stat err=%v", err)
	}

	// Second DELETE is idempotent with deleted=false.
	delReq2, _ := http.NewRequest(http.MethodDelete, auxURL, nil)
	delResp2, err := http.DefaultClient.Do(delReq2)
	if err != nil {
		t.Fatal(err)
	}
	var delBody2 struct {
		OK      bool `json:"ok"`
		Deleted bool `json:"deleted"`
	}
	_ = json.NewDecoder(delResp2.Body).Decode(&delBody2)
	_ = delResp2.Body.Close()
	if delResp2.StatusCode != http.StatusOK || !delBody2.OK || delBody2.Deleted {
		t.Fatalf("DELETE second: status=%d body=%+v", delResp2.StatusCode, delBody2)
	}

	// GET after DELETE returns 404.
	afterDelResp, err := http.Get(auxURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = afterDelResp.Body.Close()
	if afterDelResp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET after DELETE status=%d, want 404", afterDelResp.StatusCode)
	}

	// Original media file is byte-for-byte untouched.
	afterOrig, err := os.ReadFile(filepath.Join(photoDir, "IMG_0001.JPG"))
	if err != nil || !bytes.Equal(afterOrig, origBytes) {
		t.Fatalf("original media file changed (err=%v)", err)
	}
}

func TestAuxChannelColocatedReadFallbackAndScannerExclusion(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	colocatedData := samplePNG(t, 8, 8, color.RGBA{30, 60, 90, 255})
	colocatedPath := filepath.Join(photoDir, "IMG_0001.JPG.depth.png")
	if err := os.WriteFile(colocatedPath, colocatedData, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.idx.ScanAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Colocated IMG_0001.JPG.depth.png must NOT be indexed as a standalone asset.
	if code, body := getJSON(t, srv, "/assets/count"); code != http.StatusOK || body["total"] != float64(1) {
		t.Fatalf("asset count after placing colocated depth PNG: code=%d body=%v, want total=1", code, body)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()
	auxURL := ts.URL + "/assets/" + assetID + "/aux/depth"

	// GET serves colocated fallback bytes.
	resp, err := http.Get(auxURL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, colocatedData) {
		t.Fatalf("GET colocated fallback: status=%d len=%d", resp.StatusCode, len(got))
	}

	// PUT commits to .hdrtoys/ and leaves colocated file untouched.
	canonicalData := samplePNG(t, 8, 8, color.RGBA{200, 150, 100, 255})
	putReq, _ := http.NewRequest(http.MethodPut, auxURL, bytes.NewReader(canonicalData))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT status=%d", putResp.StatusCode)
	}

	// GET now prefers canonical .hdrtoys/ copy.
	resp2, err := http.Get(auxURL)
	if err != nil {
		t.Fatal(err)
	}
	got2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !bytes.Equal(got2, canonicalData) {
		t.Fatalf("GET canonical override: status=%d match=%v", resp2.StatusCode, bytes.Equal(got2, canonicalData))
	}

	// DELETE removes canonical copy but never deletes colocated copy in source folder.
	delReq, _ := http.NewRequest(http.MethodDelete, auxURL, nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status=%d", delResp.StatusCode)
	}
	stillColocated, err := os.ReadFile(colocatedPath)
	if err != nil || !bytes.Equal(stillColocated, colocatedData) {
		t.Fatalf("colocated file in source folder was altered or removed (err=%v)", err)
	}
}

func TestAuxChannelValidationAndLimits(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")

	do := func(method, target string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1"+target, body)
		req.RemoteAddr = "127.0.0.1:50000"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// 1. 415 Unsupported Media Type on non-PNG payloads.
	nonPNGPayloads := [][]byte{
		sampleJPEG(t, 16, 16, color.RGBA{10, 20, 30, 255}),
		[]byte("not-a-png"),
		{},
		{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A},
	}
	for i, payload := range nonPNGPayloads {
		rec := do(http.MethodPut, "/assets/"+assetID+"/aux/depth", bytes.NewReader(payload))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("non-PNG case %d: status=%d, want 415 (body=%s)", i, rec.Code, rec.Body.String())
		}
	}

	// 2. 413 Payload Too Large when exceeding MaxAuxChannelBytes.
	oversized := make([]byte, MaxAuxChannelBytes+1)
	copy(oversized, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'})
	rec := do(http.MethodPut, "/assets/"+assetID+"/aux/depth", bytes.NewReader(oversized))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PUT: status=%d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}

	// 3. 400 Bad Request on invalid/unknown/traversal channel names.
	badChannels := []string{
		"unknown",
		"DEPTH",
		"depth.png",
		"0depth",
		"..",
		"%2e%2e",
		"depth%2f..",
		"foo%2fbar",
	}
	validPNG := samplePNG(t, 4, 4, color.RGBA{1, 2, 3, 255})
	for _, ch := range badChannels {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			var body io.Reader
			if method == http.MethodPut {
				body = bytes.NewReader(validPNG)
			}
			rawPath := "/assets/" + assetID + "/aux/" + ch
			req := httptest.NewRequest(method, "http://127.0.0.1"+rawPath, body)
			req.RequestURI = rawPath
			req.RemoteAddr = "127.0.0.1:50000"
			r := httptest.NewRecorder()
			srv.ServeHTTP(r, req)
			if r.Code != http.StatusBadRequest {
				t.Errorf("%s %s: status=%d, want 400 (body=%s)", method, rawPath, r.Code, r.Body.String())
			}
		}
	}

	// 4. 404 Not Found for missing asset ID with valid channel.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		var body io.Reader
		if method == http.MethodPut {
			body = bytes.NewReader(validPNG)
		}
		r := do(method, "/assets/bm9zdWNoYXNzZXQ/aux/depth", body)
		if r.Code != http.StatusNotFound {
			t.Errorf("%s missing asset: status=%d, want 404", method, r.Code)
		}
	}
}

func TestAuxChannelCORSOriginAndPermissions(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	validPNG := samplePNG(t, 8, 8, color.RGBA{50, 100, 150, 255})
	target := "/assets/" + assetID + "/aux/depth"

	// 1. Disallowed origin is rejected with 403 before mutating disk.
	for _, method := range []string{http.MethodOptions, http.MethodPut, http.MethodGet, http.MethodDelete} {
		var body io.Reader
		if method == http.MethodPut {
			body = bytes.NewReader(validPNG)
		}
		req := httptest.NewRequest(method, "http://127.0.0.1"+target, body)
		req.RemoteAddr = "127.0.0.1:50000"
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s disallowed origin: status=%d, want 403", method, rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s disallowed origin leaked ACAO header", method)
		}
	}
	if _, err := os.Stat(filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.depth.png")); !os.IsNotExist(err) {
		t.Fatalf("disallowed origin PUT wrote file to disk")
	}

	// 2. Allowed origin (https://hdr.toys) receives full CORS + Headers Law including X-HDRT-Aux-Channel exposure.
	putReq := httptest.NewRequest(http.MethodPut, "http://127.0.0.1"+target, bytes.NewReader(validPNG))
	putReq.RemoteAddr = "127.0.0.1:50000"
	putReq.Header.Set("Origin", "https://hdr.toys")
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("allowed origin PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	if putRec.Header().Get("Access-Control-Allow-Origin") != "https://hdr.toys" {
		t.Fatalf("ACAO=%q, want https://hdr.toys", putRec.Header().Get("Access-Control-Allow-Origin"))
	}
	if !strings.Contains(putRec.Header().Get("Access-Control-Expose-Headers"), "X-HDRT-Aux-Channel") {
		t.Fatalf("Access-Control-Expose-Headers=%q missing X-HDRT-Aux-Channel", putRec.Header().Get("Access-Control-Expose-Headers"))
	}

	// 3. Read-only session token (asset:read only) grants GET but NOT media:write (PUT/DELETE -> 403),
	// and ?bt= query parameter credentials are rejected (401).
	readTok, _ := srv.sessions.Issue(&auth.Principal{
		Subject: "tailnet:alice",
		Scopes:  map[auth.Permission]bool{auth.PermAssetRead: true},
	})
	getReq := httptest.NewRequest(http.MethodGet, "http://library.example"+target, nil)
	getReq.RemoteAddr = "203.0.113.7:50000"
	getReq.Header.Set("Authorization", "Bearer "+readTok)
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET with read-only session token status=%d, want 200", getRec.Code)
	}

	btReq := httptest.NewRequest(http.MethodGet, "http://library.example"+target+"?bt="+readTok, nil)
	btReq.RemoteAddr = "203.0.113.7:50000"
	btRec := httptest.NewRecorder()
	srv.ServeHTTP(btRec, btReq)
	if btRec.Code != http.StatusUnauthorized {
		t.Fatalf("GET with ?bt= query token status=%d, want 401", btRec.Code)
	}

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		var body io.Reader
		if method == http.MethodPut {
			body = bytes.NewReader(validPNG)
		}
		req := httptest.NewRequest(method, "http://library.example"+target, body)
		req.RemoteAddr = "203.0.113.7:50000"
		req.Header.Set("Authorization", "Bearer "+readTok)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with read-only session token status=%d, want 403", method, rec.Code)
		}
	}

	// 4. Session with media:write can DELETE even from remote address.
	writerTok, _ := srv.sessions.Issue(&auth.Principal{
		Subject: "user:editor",
		Scopes:  map[auth.Permission]bool{auth.PermMediaWrite: true},
	})
	delReq := httptest.NewRequest(http.MethodDelete, "http://library.example"+target, nil)
	delReq.RemoteAddr = "203.0.113.7:50000"
	delReq.Header.Set("Authorization", "Bearer "+writerTok)
	delRec := httptest.NewRecorder()
	srv.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE with media:write token status=%d, want 200", delRec.Code)
	}
}
