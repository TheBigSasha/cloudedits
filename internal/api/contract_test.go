package api

import (
	"bytes"
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestPreflightAllowsEveryEditorRequestHeader pins that a browser preflight
// admits every non-safelisted request header the editor attaches to Remote
// Library calls. A header missing from Access-Control-Allow-Headers makes the
// browser block the request before it is sent.
func TestPreflightAllowsEveryEditorRequestHeader(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")
	req := httptest.NewRequest(http.MethodOptions, "/assets/"+assetID+"/renders", nil)
	req.Header.Set("Origin", "https://hdr.toys")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	allowed := map[string]bool{}
	for _, h := range strings.Split(rec.Header().Get("Access-Control-Allow-Headers"), ",") {
		allowed[strings.ToLower(strings.TrimSpace(h))] = true
	}
	for _, h := range []string{
		"Authorization", "Content-Type", "Range", "If-Range", "If-None-Match",
		"Idempotency-Key", "X-HDRT-Profile",
		"X-HDRToys-Client", "X-HDRToys-Image-Id", "X-HDRToys-Render-Id",
		"X-HDRToys-Variant-Id", "X-HDRToys-Channel", "X-HDRToys-Filename",
		"X-HDRToys-Schema-Version",
	} {
		if !allowed[strings.ToLower(h)] {
			t.Errorf("preflight does not allow request header %q", h)
		}
	}
}

// TestRenderUploadReadsVariantFromHeader pins that the variant a render
// belongs to is taken from the X-HDRToys-Variant-Id request header when the
// variantId query parameter is absent.
func TestRenderUploadReadsVariantFromHeader(t *testing.T) {
	srv, _, assetID := newTestServer(t, t.TempDir())
	body := bytes.NewReader(sampleJPEG(t, 32, 24, color.RGBA{10, 20, 30, 255}))
	req := httptest.NewRequest(http.MethodPost, "/assets/"+assetID+"/renders", body)
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Idempotency-Key", "render-hdr-1")
	req.Header.Set("X-HDRToys-Variant-Id", "variant-7")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var out struct {
		Render struct {
			VariantID string `json:"variantId"`
		} `json:"render"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("code=%d err=%v body=%s", rec.Code, err, rec.Body.String())
	}
	if out.Render.VariantID != "variant-7" {
		t.Errorf("render variantId = %q, want %q", out.Render.VariantID, "variant-7")
	}
}

// TestOversizedOpsBodyIsPayloadTooLarge pins that a POST /ops body over the
// byte limit is answered with 413 payload-too-large (not a generic 400 a
// client cannot tell apart from malformed JSON), and that the limit a client
// must stay under is advertised in /capabilities.
func TestOversizedOpsBodyIsPayloadTooLarge(t *testing.T) {
	srv, _, _ := newTestServer(t, "")

	_, caps := getJSON(t, srv, "/capabilities")
	limits, _ := caps["limits"].(map[string]any)
	advertised, ok := limits["maxOpsBodyBytes"].(float64)
	if !ok || advertised <= 0 {
		t.Fatalf("limits.maxOpsBodyBytes = %v, want the POST /ops byte limit", limits["maxOpsBodyBytes"])
	}

	post := func(size int) (int, string) {
		pad := strings.Repeat("a", size)
		body := `[{"hlc":"019a2f001000-0001-d","device":"d","assetKey":"missing","field":"keywords","value":["` + pad + `"]}]`
		req := httptest.NewRequest(http.MethodPost, "/ops", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:50000"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		var out struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Error.Code
	}

	if code, errCode := post(int(advertised) + 1); code != http.StatusRequestEntityTooLarge || errCode != "payload-too-large" {
		t.Errorf("body over the advertised limit: %d %q, want 413 payload-too-large", code, errCode)
	}
	if code, _ := post(1024); code == http.StatusRequestEntityTooLarge {
		t.Errorf("small body was rejected as too large (%d)", code)
	}
}
