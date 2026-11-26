package thumb

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// The fast decoder must treat a marker as the end of entropy data. A scan cut
// short and closed with EOI is corrupt: the fast path has to fail (so the
// standard decoder's verdict stands) instead of fabricating the missing
// blocks from replayed bytes and reporting success.
func TestFastDecodeRejectsScanTruncatedBeforeEOI(t *testing.T) {
	const w, h = 1600, 1200
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x ^ y), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	full := buf.Bytes()
	if _, err := decodeJPEGScale8Bytes(full, 0); err != nil {
		t.Fatalf("intact JPEG must decode on the fast path: %v", err)
	}
	for _, frac := range []int{2, 3, 4, 7, 10, 50} {
		cut := append([]byte{}, full[:len(full)/frac]...)
		for len(cut) > 0 && cut[len(cut)-1] == 0xFF {
			cut = cut[:len(cut)-1]
		}
		cut = append(cut, 0xFF, 0xD9)
		if _, err := jpeg.Decode(bytes.NewReader(cut)); err == nil {
			t.Fatalf("test premise: stdlib accepted the truncated scan (1/%d)", frac)
		}
		if img, err := decodeJPEGScale8Bytes(cut, 0); err == nil {
			t.Fatalf("scan truncated to 1/%d decoded \"successfully\" to %v", frac, img.Bounds())
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "truncated.jpg")
		padded := append(append([]byte{}, cut...), make([]byte, 200*1024)...) // reach the fast-path size floor
		if err := os.WriteFile(path, padded, 0o644); err != nil {
			t.Fatal(err)
		}
		if res, err := NewService(filepath.Join(dir, "cache"), 1).Resolve(context.Background(), path, "rev-trunc", "", "t0"); err == nil {
			t.Fatalf("truncated JPEG (1/%d) produced a %d-byte thumbnail instead of an error", frac, len(res.Data))
		}
	}
}

// Restart intervals exercise the marker handling the fix above touches. The
// same picture encoded with and without restart markers carries identical DC
// coefficients, so the 1/8-scale outputs must match exactly.
func TestFastDecodeHandlesRestartIntervals(t *testing.T) {
	decode := func(name string) image.Image {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		img, err := decodeJPEGScale8Bytes(data, 0)
		if err != nil {
			t.Fatalf("%s: fast decode: %v", name, err)
		}
		return img
	}
	for _, kind := range []string{"420", "gray"} {
		plain, restart := decode("plain_"+kind+".jpg"), decode("restart_"+kind+".jpg")
		if plain.Bounds() != restart.Bounds() || plain.Bounds().Dx() != 20 || plain.Bounds().Dy() != 15 {
			t.Fatalf("%s: bounds %v vs %v, want 20x15", kind, plain.Bounds(), restart.Bounds())
		}
		for y := 0; y < 15; y++ {
			for x := 0; x < 20; x++ {
				if plain.At(x, y) != restart.At(x, y) {
					t.Fatalf("%s: pixel (%d,%d) differs: plain %v, with restart markers %v", kind, x, y, plain.At(x, y), restart.At(x, y))
				}
			}
		}
	}
}
