package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thebigsasha/cloudedits/internal/auth"
	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/handoff"
	"github.com/thebigsasha/cloudedits/internal/index"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
	"github.com/thebigsasha/cloudedits/internal/thumb"
)

func sampleJPEG(t testing.TB, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTestServer(t testing.TB, sidecarDir string) (*Server, string, string) {
	t.Helper()
	photoDir := t.TempDir()
	dataDir := t.TempDir()

	jpgBytes := sampleJPEG(t, 800, 600, color.RGBA{180, 90, 40, 255})
	if err := os.WriteFile(filepath.Join(photoDir, "IMG_0001.JPG"), jpgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photoDir, "._IMG_0001.JPG"), jpgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	folders := []config.FolderConfig{{ID: "main", Path: photoDir, Label: "Main"}}
	idx, err := index.Open(filepath.Join(dataDir, "index.db"), sidecarDir, folders, sidecar.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.ScanAll(context.Background()); err != nil {
		t.Fatal(err)
	}

	sessions := auth.NewSessionManager(15 * time.Minute)
	srv, err := New(Options{
		LibraryID:   "test-lib",
		LibraryName: "Test Library",
		Index:       idx,
		Thumbs:      thumb.NewService(filepath.Join(dataDir, "thumbs"), 2),
		Handoffs:    handoff.NewStore(5 * time.Minute),
		Sessions:    sessions,
		Auth: &auth.CompositeAuthenticator{
			Sessions:      sessions,
			StaticToken:   "tok-123",
			AllowLoopback: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv, photoDir, index.MakeAssetID("main", "IMG_0001.JPG")
}

func TestValidateRoutesFailClosed(t *testing.T) {
	noop := func(http.ResponseWriter, *http.Request) {}

	if err := ValidateRoutes([]RouteSpec{
		{Method: http.MethodGet, Pattern: "/x", Auth: auth.AuthUnset, SLOBudgetMs: 10, Handler: noop},
	}, false); err == nil {
		t.Fatal("expected failure on unset AuthPolicy")
	}

	if err := ValidateRoutes([]RouteSpec{
		{Method: http.MethodGet, Pattern: "/x", Auth: auth.AuthAuthenticated, Permission: auth.PermNone, SLOBudgetMs: 10, Handler: noop},
	}, false); err == nil {
		t.Fatal("expected failure on authenticated route without Permission")
	}

	if err := ValidateRoutes([]RouteSpec{
		{Method: http.MethodPost, Pattern: "/__dev/reset", Auth: auth.AuthDevOnly, SLOBudgetMs: 10, Handler: noop},
	}, false); err == nil {
		t.Fatal("expected failure on DevOnly route when allowDevOnly=false")
	}

	if err := ValidateRoutes([]RouteSpec{
		{Method: http.MethodPost, Pattern: "/__dev/reset", Auth: auth.AuthDevOnly, SLOBudgetMs: 10, Handler: noop},
	}, true); err != nil {
		t.Fatalf("expected DevOnly route valid when allowDevOnly=true: %v", err)
	}
}

func TestHeadersLawRangeOpsEditThumbnailAndHandoff(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Capabilities + Headers Law.
	resp, err := http.Get(ts.URL + "/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities status=%d", resp.StatusCode)
	}
	if resp.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("missing CORP header")
	}
	if !strings.Contains(resp.Header.Get("Server-Timing"), "app;dur=") {
		t.Fatalf("missing Server-Timing header")
	}

	// 2. Original blob with Range 206, If-Range, no-store, identity, strong ETag.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/assets/"+assetID+"/blob?tier=original", nil)
	req.Header.Set("Range", "bytes=0-63")
	origResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = origResp.Body.Close()
	if origResp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", origResp.StatusCode)
	}
	if origResp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("unexpected Cache-Control: %q", origResp.Header.Get("Cache-Control"))
	}
	if origResp.Header.Get("Content-Encoding") != "identity" {
		t.Fatalf("unexpected Content-Encoding: %q", origResp.Header.Get("Content-Encoding"))
	}
	if origResp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("unexpected Accept-Ranges: %q", origResp.Header.Get("Accept-Ranges"))
	}
	etag := origResp.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("expected strong quoted ETag, got %q", etag)
	}

	// If-Range mismatch must fall back to 200 OK full file.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/assets/"+assetID+"/file?tier=original", nil)
	req2.Header.Set("Range", "bytes=0-63")
	req2.Header.Set("If-Range", `"stale-etag"`)
	mismatchResp, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = mismatchResp.Body.Close()
	if mismatchResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on mismatched If-Range, got %d", mismatchResp.StatusCode)
	}

	// 3. POST /ops writes atomic .hdrtoys/IMG_0001.JPG.edits and exposes /changes + /recipe + /variants.
	opsBody := `{"ops":[
		{"hlc":"019a2f001000-0001-dev1","device":"dev1","assetKey":"` + assetID + `","field":"rating","value":5},
		{"hlc":"019a2f001000-0002-dev1","device":"dev1","assetKey":"` + assetID + `","field":"recipe","value":{"exposure":0.75}},
		{"hlc":"019a2f001000-0003-dev1","device":"dev1","assetKey":"` + assetID + `","field":"variant","value":{"id":"v1","name":"Sunset","recipe":{"exposure":0.75}}}
	]}`
	opsResp, err := http.Post(ts.URL+"/ops", "application/json", strings.NewReader(opsBody))
	if err != nil {
		t.Fatal(err)
	}
	var opsRes struct {
		Durable  bool              `json:"durable"`
		Receipts []index.OpReceipt `json:"receipts"`
	}
	if err := json.NewDecoder(opsResp.Body).Decode(&opsRes); err != nil {
		t.Fatal(err)
	}
	_ = opsResp.Body.Close()
	if !opsRes.Durable || len(opsRes.Receipts) != 3 {
		t.Fatalf("unexpected ops result: %+v", opsRes)
	}
	if _, err := os.Stat(filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.edits")); err != nil {
		t.Fatalf("expected .hdrtoys sidecar on disk: %v", err)
	}

	// GET /changes returns the 3 ops.
	chResp, err := http.Get(ts.URL + "/changes")
	if err != nil {
		t.Fatal(err)
	}
	var feed index.ChangesFeed
	_ = json.NewDecoder(chResp.Body).Decode(&feed)
	_ = chResp.Body.Close()
	if len(feed.Ops) != 3 {
		t.Fatalf("expected 3 change ops, got %d", len(feed.Ops))
	}

	// 4. PUT /assets/{id}/edit-thumbnail persists and takes tier priority.
	thumbData := sampleJPEG(t, 200, 150, color.RGBA{10, 220, 90, 255})
	putReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/assets/"+assetID+"/edit-thumbnail", bytes.NewReader(thumbData))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("edit-thumbnail PUT status=%d", putResp.StatusCode)
	}

	t1Resp, err := http.Get(ts.URL + "/assets/" + assetID + "/blob?tier=t1")
	if err != nil {
		t.Fatal(err)
	}
	_ = t1Resp.Body.Close()
	if t1Resp.Header.Get("X-HDRT-Tier-Source") != "edit-thumbnail" {
		t.Fatalf("expected edit-thumbnail tier source, got %q", t1Resp.Header.Get("X-HDRT-Tier-Source"))
	}

	// 5. POST /handoff -> GET /handoff/{id} -> GET /h/{id} 302 redirect.
	hoResp, err := http.Post(ts.URL+"/handoff", "application/json", strings.NewReader(`{"v":1,"assetId":"`+assetID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var ho struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.NewDecoder(hoResp.Body).Decode(&ho)
	_ = hoResp.Body.Close()
	if len(ho.ID) != 8 {
		t.Fatalf("expected 8-char handoff ID, got %q", ho.ID)
	}

	getHo, err := http.Get(ts.URL + "/handoff/" + ho.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = getHo.Body.Close()
	if getHo.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on GET /handoff/%s, got %d", ho.ID, getHo.StatusCode)
	}

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redir, err := noFollow.Get(ts.URL + "/h/" + ho.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = redir.Body.Close()
	if redir.StatusCode != http.StatusFound || !strings.Contains(redir.Header.Get("Location"), "handoff="+ho.ID) {
		t.Fatalf("expected 302 redirect with handoff=%s, got %d loc=%q", ho.ID, redir.StatusCode, redir.Header.Get("Location"))
	}

	// 6. GET / redirect, /open redirect, /assets/range?fields=grid, /assets cursor, /recipe, /variants, DELETE variant, /renders idempotency.
	rootRedir, err := noFollow.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = rootRedir.Body.Close()
	if rootRedir.StatusCode != http.StatusFound || !strings.Contains(rootRedir.Header.Get("Location"), "gallery_api_url=") {
		t.Fatalf("expected 302 on GET /, got %d loc=%q", rootRedir.StatusCode, rootRedir.Header.Get("Location"))
	}

	openRedir, err := noFollow.Get(ts.URL + "/open?a=" + assetID)
	if err != nil {
		t.Fatal(err)
	}
	_ = openRedir.Body.Close()
	if openRedir.StatusCode != http.StatusFound || !strings.Contains(openRedir.Header.Get("Location"), "image_id="+assetID) {
		t.Fatalf("expected 302 on GET /open, got %d", openRedir.StatusCode)
	}

	rangeResp, err := http.Get(ts.URL + "/assets/range?offset=0&limit=10&fields=grid")
	if err != nil {
		t.Fatal(err)
	}
	var rangeBody struct {
		Total  int               `json:"total"`
		Exact  bool              `json:"exact"`
		Assets []index.GridAsset `json:"assets"`
	}
	_ = json.NewDecoder(rangeResp.Body).Decode(&rangeBody)
	_ = rangeResp.Body.Close()
	if rangeBody.Total != 1 || !rangeBody.Exact || len(rangeBody.Assets) != 1 || rangeBody.Assets[0].Rating != 5 || !rangeBody.Assets[0].EditedByHdrToys {
		t.Fatalf("unexpected range result: %+v", rangeBody)
	}

	// Single asset carries thumbnailUrl, rawUrl, recipeUrl, editedByHdrToys, and full exif.
	singleResp, err := http.Get(ts.URL + "/assets/" + assetID)
	if err != nil {
		t.Fatal(err)
	}
	var single index.Asset
	_ = json.NewDecoder(singleResp.Body).Decode(&single)
	_ = singleResp.Body.Close()
	if single.ThumbnailURL == "" || single.RawURL == "" || single.RecipeURL == "" || !single.EditedByHdrToys || len(single.EXIF) == 0 {
		t.Fatalf("missing URLs/exif/editedByHdrToys on single asset: %+v", single)
	}

	// Non-tombstoned variant includes name, recipeUrl, and serves GET /assets/{id}/variants/{variantId}/recipe.
	varBeforeResp, err := http.Get(ts.URL + "/assets/" + assetID + "/variants")
	if err != nil {
		t.Fatal(err)
	}
	var varBefore struct {
		Variants []map[string]any `json:"variants"`
	}
	_ = json.NewDecoder(varBeforeResp.Body).Decode(&varBefore)
	_ = varBeforeResp.Body.Close()
	if len(varBefore.Variants) != 1 || varBefore.Variants[0]["name"] != "Sunset" || !strings.HasSuffix(varBefore.Variants[0]["recipeUrl"].(string), "/variants/v1/recipe") {
		t.Fatalf("unexpected variants payload before tombstone: %+v", varBefore.Variants)
	}
	vrResp, err := http.Get(varBefore.Variants[0]["recipeUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	_ = vrResp.Body.Close()
	if vrResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on variant recipeUrl, got %d", vrResp.StatusCode)
	}

	// Reject non-JPEG edit-thumbnail with 415.
	badThumbReq, _ := http.NewRequest(http.MethodPut, ts.URL+"/assets/"+assetID+"/edit-thumbnail", strings.NewReader("not-a-jpeg"))
	badThumbResp, err := http.DefaultClient.Do(badThumbReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badThumbResp.Body.Close()
	if badThumbResp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for non-JPEG edit-thumbnail, got %d", badThumbResp.StatusCode)
	}

	// Render upload idempotency + list.
	rReq1, _ := http.NewRequest(http.MethodPost, ts.URL+"/assets/"+assetID+"/renders?variantId=v1", bytes.NewReader(thumbData))
	rReq1.Header.Set("Idempotency-Key", "render-001")
	rReq1.Header.Set("Content-Type", "image/jpeg")
	rResp1, err := http.DefaultClient.Do(rReq1)
	if err != nil {
		t.Fatal(err)
	}
	var r1 struct {
		Created bool `json:"created"`
	}
	_ = json.NewDecoder(rResp1.Body).Decode(&r1)
	_ = rResp1.Body.Close()
	if !r1.Created {
		t.Fatalf("expected created=true on first render upload")
	}

	rReq2, _ := http.NewRequest(http.MethodPost, ts.URL+"/assets/"+assetID+"/renders?variantId=v1", bytes.NewReader(thumbData))
	rReq2.Header.Set("Idempotency-Key", "render-001")
	rReq2.Header.Set("Content-Type", "image/jpeg")
	rResp2, err := http.DefaultClient.Do(rReq2)
	if err != nil {
		t.Fatal(err)
	}
	var r2 struct {
		Created bool `json:"created"`
	}
	_ = json.NewDecoder(rResp2.Body).Decode(&r2)
	_ = rResp2.Body.Close()
	if r2.Created {
		t.Fatalf("expected created=false on duplicate Idempotency-Key render upload")
	}

	// DELETE variant tombstones it cleanly.
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/assets/"+assetID+"/variants/v1", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on DELETE variant, got %d", delResp.StatusCode)
	}

	// After tombstoning, default GET /variants filters out tombstones; ?includeTombstones=true returns them.
	varResp, err := http.Get(ts.URL + "/assets/" + assetID + "/variants")
	if err != nil {
		t.Fatal(err)
	}
	var varBody struct {
		Variants []map[string]any `json:"variants"`
	}
	_ = json.NewDecoder(varResp.Body).Decode(&varBody)
	_ = varResp.Body.Close()
	if len(varBody.Variants) != 0 {
		t.Fatalf("expected tombstoned variant filtered out by default, got %+v", varBody.Variants)
	}

	varAllResp, err := http.Get(ts.URL + "/assets/" + assetID + "/variants?includeTombstones=true")
	if err != nil {
		t.Fatal(err)
	}
	var varAllBody struct {
		Variants []map[string]any `json:"variants"`
	}
	_ = json.NewDecoder(varAllResp.Body).Decode(&varAllBody)
	_ = varAllResp.Body.Close()
	if len(varAllBody.Variants) != 1 || varAllBody.Variants[0]["tombstone"] != true {
		t.Fatalf("expected tombstoned v1 when includeTombstones=true, got %+v", varAllBody.Variants)
	}

	// POST /auth/session issues a session bearer token and no library-wide blobToken (O4).
	sesResp, err := http.Post(ts.URL+"/auth/session", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sesBody map[string]any
	_ = json.NewDecoder(sesResp.Body).Decode(&sesBody)
	_ = sesResp.Body.Close()
	if tok, _ := sesBody["token"].(string); tok == "" {
		t.Fatalf("expected session token, got %+v", sesBody)
	}
	if _, hasBlob := sesBody["blobToken"]; hasBlob {
		t.Fatalf("POST /auth/session must not mint unused blobToken, got %+v", sesBody)
	}
}

func TestCorruptLargeDHTJPEGReturns422AndPathTraversalRejected(t *testing.T) {
	srv, photoDir, _ := newTestServer(t, "")

	// Write a >2 MiB 4096x4096 malformed over-subscribed DHT JPEG into the library.
	var bad bytes.Buffer
	bad.Write([]byte{0xFF, 0xD8})
	bad.Write([]byte{0xFF, 0xC0, 0x00, 11, 8, 0x10, 0x00, 0x10, 0x00, 1, 1, 0x11, 0})
	bad.Write([]byte{0xFF, 0xC4, 0x00, 22, 0x00})
	bad.Write([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	bad.Write([]byte{0x00, 0x01, 0x02})
	bad.Write([]byte{0xFF, 0xDA, 0x00, 8, 1, 1, 0x00, 0, 63, 0})
	bad.Write(bytes.Repeat([]byte{0x55}, 2*1024*1024+1024))
	bad.Write([]byte{0xFF, 0xD9})
	if err := os.WriteFile(filepath.Join(photoDir, "corrupt_4096_dht.jpg"), bad.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.idx.ScanAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	corruptID := index.MakeAssetID("main", "corrupt_4096_dht.jpg")

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Corrupt >2 MiB JPEG must return HTTP 422 (never HTTP 200 raw bytes).
	resp, err := http.Get(ts.URL + "/assets/" + corruptID + "/blob?tier=t1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected HTTP 422 for corrupt >2 MiB DHT JPEG, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("missing CORP header on 422 error response")
	}

	// 2. Raw uncleaned path traversal must return HTTP 400 (never 501 or 301 redirect).
	for _, rawPath := range []string{
		"/assets/../../../etc/passwd",
		"/assets/%2e%2e/%2e%2e/etc/passwd",
		"/assets/..%2f..%2fetc/passwd",
		"/assets/..\\..\\etc\\passwd",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1"+rawPath, nil)
		req.RequestURI = rawPath
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("path %q: expected HTTP 400, got %d body=%s", rawPath, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" {
			t.Fatalf("path %q: missing CORP header on 400 response", rawPath)
		}
	}

	// 3. Verify raw TCP HTTP/1.1 wire request GET /assets/../../../etc/passwd returns 400 (not 301/501).
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /assets/../../../etc/passwd HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	rawResp, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rawResp), "HTTP/1.1 400 ") {
		t.Fatalf("expected raw wire HTTP/1.1 400 for /assets/../../../etc/passwd, got:\n%s", string(rawResp))
	}
}
