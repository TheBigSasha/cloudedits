//go:build !windows

package api

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}

// An auxiliary channel is read from two places other software can write to:
// the sidecar folder and the photo folder itself. Whatever is found there must
// not be trusted: a symbolic link must not disclose another file, a device or
// oversized file must not be read into memory, and bytes that are not a PNG
// must not be served as one.
func TestAuxChannelReadTrustsOnlyBoundedRegularPNGs(t *testing.T) {
	srv, photoDir, assetID := newTestServer(t, "")
	outside := t.TempDir()
	url := "/assets/" + assetID + "/aux/depth"
	beside := filepath.Join(photoDir, "IMG_0001.JPG.depth.png")
	canonical := filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.depth.png")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}

	secret := filepath.Join(outside, "id_ed25519")
	secretPNG := filepath.Join(outside, "private.png")
	if err := os.WriteFile(secret, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nSECRET-KEY-MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPNG, append(append([]byte{}, pngSignature...), []byte("SECRET-PNG-OUTSIDE-LIBRARY")...), 0o600); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(outside, "oversized.png")
	f, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(pngSignature); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxAuxChannelBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	plant := func(t *testing.T, at string, link bool, source string) {
		t.Helper()
		_ = os.Remove(beside)
		_ = os.Remove(canonical)
		if link {
			if err := os.Symlink(source, at); err != nil {
				t.Fatal(err)
			}
			return
		}
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		at     string
		link   bool
		source string
	}{
		{"link beside photo to a private file", beside, true, secret},
		{"link beside photo to a PNG outside the library", beside, true, secretPNG},
		{"link in sidecar folder to a private file", canonical, true, secret},
		{"link in sidecar folder to a PNG outside the library", canonical, true, secretPNG},
		{"link beside photo to an endless device", beside, true, "/dev/zero"},
		{"oversized file beside photo", beside, false, oversized},
		{"oversized file in sidecar folder", canonical, false, oversized},
		{"non-PNG file beside photo", beside, false, secret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.source == "/dev/zero" && !auxReadIsBounded(t, srv, photoDir, assetID) {
				t.Fatal("unbounded read path: not exercising /dev/zero, it would exhaust memory")
			}
			plant(t, tc.at, tc.link, tc.source)
			rec := probe{path: url}.do(srv)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status %d (%d bytes, Content-Type %q), want 404", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Type"))
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("SECRET")) {
				t.Fatalf("response discloses a file outside the library: %.80q", rec.Body.String())
			}
		})
	}

	// A genuine channel, written by a local editor beside the photo or uploaded
	// through the API, is still served.
	real := append(append([]byte{}, pngSignature...), bytes.Repeat([]byte{7}, 256)...)
	_ = os.Remove(beside)
	_ = os.Remove(canonical)
	if err := os.WriteFile(beside, real, 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := (probe{path: url}).do(srv); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), real) {
		t.Fatalf("genuine channel beside photo: status %d, %d bytes", rec.Code, rec.Body.Len())
	}
	_ = os.Remove(beside)
	if rec := (probe{method: http.MethodPut, path: url, body: string(real)}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("upload: status %d %s", rec.Code, rec.Body.String())
	}
	if rec := (probe{path: url}).do(srv); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), real) {
		t.Fatalf("uploaded channel: status %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

// auxReadIsBounded reports whether the read path refuses an oversized regular
// file, which is the precondition for safely pointing it at /dev/zero.
func auxReadIsBounded(t *testing.T, srv *Server, photoDir, assetID string) bool {
	t.Helper()
	path := filepath.Join(photoDir, "IMG_0001.JPG.depth.png")
	_ = os.Remove(path)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(pngSignature)
	_ = f.Truncate(MaxAuxChannelBytes + 1)
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()
	asset, err := srv.idx.GetAsset(t.Context(), assetID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = srv.idx.Sidecars().ReadAuxChannel(asset.FolderRoot, asset.SidecarRoot, asset.RelPath, "depth")
	return err != nil
}
