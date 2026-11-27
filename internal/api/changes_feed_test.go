package api

import (
	"bytes"
	"encoding/json"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type feedPage struct {
	Ops []struct {
		Seq     int64  `json:"seq"`
		Device  string `json:"device"`
		HLC     string `json:"hlc"`
		AssetID string `json:"assetId"`
		Field   string `json:"field"`
		Outcome string `json:"outcome"`
	} `json:"ops"`
	Assets []struct {
		ID           string `json:"id"`
		ThumbnailURL string `json:"thumbnailUrl"`
	} `json:"assets"`
	NextCursor       string `json:"nextCursor"`
	SnapshotRequired bool   `json:"snapshotRequired"`
}

func getFeed(t *testing.T, srv *Server, since string) feedPage {
	t.Helper()
	target := "http://127.0.0.1/changes"
	if since != "" {
		target += "?since=" + url.QueryEscape(since)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "127.0.0.1:50000"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var page feedPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("GET /changes?since=%s: code=%d err=%v body=%s", since, rec.Code, err, rec.Body.String())
	}
	return page
}

// A client that has just listed the assets asks for the journal head and
// polls from there. An edited preview uploaded by another client must then
// arrive as a feed entry, and the asset's thumbnail URL must change with it:
// the old URL is cached by every browser that already showed the thumbnail.
func TestUploadedEditThumbnailReachesTheFeedWithANewThumbnailURL(t *testing.T) {
	srv, _, assetID := newTestServer(t, "")

	_, before := getJSON(t, srv, "/assets/"+assetID)
	urlBefore, _ := before["thumbnailUrl"].(string)
	parsed, err := url.Parse(urlBefore)
	if err != nil || parsed.Query().Get("v") == "" || parsed.Query().Get("tier") != "thumb" {
		t.Fatalf("thumbnailUrl = %q, want a versioned tier=thumb URL", urlBefore)
	}

	head := getFeed(t, srv, "head")
	if len(head.Ops) != 0 || head.SnapshotRequired || head.NextCursor == "" {
		t.Fatalf("since=head: %+v, want an empty page carrying the head cursor", head)
	}

	put := httptest.NewRequest(http.MethodPut, "http://127.0.0.1/assets/"+assetID+"/edit-thumbnail",
		bytes.NewReader(sampleJPEG(t, 64, 48, color.RGBA{10, 220, 90, 255})))
	put.RemoteAddr = "127.0.0.1:50000"
	put.Header.Set("Content-Type", "image/jpeg")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, put)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT edit-thumbnail: %d %s", rec.Code, rec.Body.String())
	}

	page := getFeed(t, srv, head.NextCursor)
	if len(page.Ops) != 1 || page.Ops[0].Field != "editThumbnail" || page.Ops[0].Outcome != "observed" || page.Ops[0].AssetID != assetID {
		t.Fatalf("feed after the upload: %+v, want one observed editThumbnail entry", page.Ops)
	}
	if page.Ops[0].Device == "" || page.Ops[0].HLC == "" {
		t.Errorf("observed entry lacks device/hlc: %+v", page.Ops[0])
	}
	if len(page.Assets) != 1 || page.Assets[0].ThumbnailURL == urlBefore {
		t.Fatalf("feed asset thumbnailUrl = %+v, want a URL different from %q", page.Assets, urlBefore)
	}

	// The versioned URL is served, and the list reports the same URL.
	thumb := httptest.NewRequest(http.MethodGet, page.Assets[0].ThumbnailURL, nil)
	thumb.RemoteAddr = "127.0.0.1:50000"
	thumbRec := httptest.NewRecorder()
	srv.ServeHTTP(thumbRec, thumb)
	if thumbRec.Code != http.StatusOK || thumbRec.Header().Get("X-HDRT-Tier-Source") != "edit-thumbnail" {
		t.Fatalf("GET versioned thumbnail: %d source=%q", thumbRec.Code, thumbRec.Header().Get("X-HDRT-Tier-Source"))
	}
	_, after := getJSON(t, srv, "/assets/"+assetID)
	if after["thumbnailUrl"] != page.Assets[0].ThumbnailURL {
		t.Errorf("GET /assets/{id} thumbnailUrl = %v, feed says %q", after["thumbnailUrl"], page.Assets[0].ThumbnailURL)
	}
}
