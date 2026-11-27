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
	besideJPG := filepath.Join(photoDir, "IMG_0001.JPG.depth.jpg")
	canonical := filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.depth.png")
	canonicalJPG := filepath.Join(photoDir, ".hdrtoys", "IMG_0001.JPG.depth.jpg")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}

	jpegSignature := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	secret := filepath.Join(outside, "id_ed25519")
	secretPNG := filepath.Join(outside, "private.png")
	secretJPG := filepath.Join(outside, "private.jpg")
	if err := os.WriteFile(secret, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nSECRET-KEY-MATERIAL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPNG, append(append([]byte{}, pngSignature...), []byte("SECRET-PNG-OUTSIDE-LIBRARY")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretJPG, append(append([]byte{}, jpegSignature...), []byte("SECRET-JPG-OUTSIDE-LIBRARY")...), 0o600); err != nil {
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

	oversizedJPG := filepath.Join(outside, "oversized.jpg")
	fj, err := os.Create(oversizedJPG)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fj.Write(jpegSignature); err != nil {
		t.Fatal(err)
	}
	if err := fj.Truncate(MaxAuxChannelBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = fj.Close()

	plant := func(t *testing.T, at string, link bool, source string) {
		t.Helper()
		_ = os.Remove(beside)
		_ = os.Remove(besideJPG)
		_ = os.Remove(canonical)
		_ = os.Remove(canonicalJPG)
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
		{"link beside photo (.depth.jpg) to a JPEG outside the library", besideJPG, true, secretJPG},
		{"link in sidecar folder to a private file", canonical, true, secret},
		{"link in sidecar folder to a PNG outside the library", canonical, true, secretPNG},
		{"link in sidecar folder (.depth.jpg) to a JPEG outside the library", canonicalJPG, true, secretJPG},
		{"link beside photo to an endless device", beside, true, "/dev/zero"},
		{"oversized file beside photo", beside, false, oversized},
		{"oversized JPEG beside photo", besideJPG, false, oversizedJPG},
		{"oversized file in sidecar folder", canonical, false, oversized},
		{"oversized JPEG in sidecar folder", canonicalJPG, false, oversizedJPG},
		{"non-PNG file beside photo", beside, false, secret},
		{"non-JPEG file beside photo (.depth.jpg)", besideJPG, false, secret},
		{"PNG bytes masquerading as .depth.jpg", canonicalJPG, false, secretPNG},
		{"JPEG bytes masquerading as .depth.png", canonical, false, secretJPG},
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

	// Genuine PNG and JPEG channels, written beside the photo or uploaded through
	// the API, are served with matching Content-Type.
	realPNG := append(append([]byte{}, pngSignature...), bytes.Repeat([]byte{7}, 256)...)
	realJPG := append(append([]byte{}, jpegSignature...), bytes.Repeat([]byte{9}, 256)...)
	_ = os.Remove(beside)
	_ = os.Remove(besideJPG)
	_ = os.Remove(canonical)
	_ = os.Remove(canonicalJPG)
	if err := os.WriteFile(beside, realPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := (probe{path: url}).do(srv); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), realPNG) {
		t.Fatalf("genuine PNG beside photo: status %d, ct %q, %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	_ = os.Remove(beside)
	if err := os.WriteFile(besideJPG, realJPG, 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := (probe{path: url}).do(srv); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), realJPG) {
		t.Fatalf("genuine JPEG beside photo: status %d, ct %q, %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	_ = os.Remove(besideJPG)
	if rec := (probe{method: http.MethodPut, path: url, body: string(realJPG)}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("JPEG upload: status %d %s", rec.Code, rec.Body.String())
	}
	if rec := (probe{path: url}).do(srv); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(rec.Body.Bytes(), realJPG) {
		t.Fatalf("uploaded JPEG channel: status %d, ct %q, %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
	if rec := (probe{method: http.MethodPut, path: url, body: string(realPNG)}).do(srv); rec.Code != http.StatusOK {
		t.Fatalf("PNG upload: status %d %s", rec.Code, rec.Body.String())
	}
	if rec := (probe{path: url}).do(srv); rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), realPNG) {
		t.Fatalf("uploaded PNG channel: status %d, ct %q, %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
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
