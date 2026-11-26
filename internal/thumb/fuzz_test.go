package thumb

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/image/tiff"
)

// fuzzSeedJPEGs returns small valid baseline JPEGs covering grayscale and
// 4:2:0 colour plus the two historical crash-class inputs (over-subscribed
// DHT and zero SOF0 dimensions).
func fuzzSeedJPEGs(tb testing.TB) [][]byte {
	tb.Helper()
	var seeds [][]byte

	rgba := image.NewRGBA(image.Rect(0, 0, 96, 64))
	gray := image.NewGray(image.Rect(0, 0, 72, 40))
	for y := 0; y < 64; y++ {
		for x := 0; x < 96; x++ {
			rgba.SetRGBA(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 3), B: uint8(x + y), A: 255})
			if x < 72 && y < 40 {
				gray.SetGray(x, y, color.Gray{Y: uint8(x*3 + y)})
			}
		}
	}
	for _, img := range []image.Image{rgba, gray} {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
			tb.Fatal(err)
		}
		seeds = append(seeds, buf.Bytes())
	}

	var overSub bytes.Buffer
	overSub.Write([]byte{0xFF, 0xD8, 0xFF, 0xC4, 0x00, 22, 0x00})
	overSub.Write([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	overSub.Write([]byte{0x00, 0x01, 0x02})
	overSub.Write(make([]byte, 48))
	overSub.Write([]byte{0xFF, 0xD9})
	seeds = append(seeds, overSub.Bytes())

	var zeroDims bytes.Buffer
	zeroDims.Write([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 11, 8, 0, 0, 0, 0, 1, 1, 0x11, 0})
	zeroDims.Write(make([]byte, 64))
	zeroDims.Write([]byte{0xFF, 0xD9})
	seeds = append(seeds, zeroDims.Bytes())
	return seeds
}

// FuzzDecodeJPEGScale8 exercises the hand-written 1/8-scale baseline JPEG
// decoder. It must never panic, and whenever both it and the standard library
// accept an input they must agree on the 1/8-scale output dimensions.
func FuzzDecodeJPEGScale8(f *testing.F) {
	for _, s := range fuzzSeedJPEGs(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if w, h := parseJPEGDimensions(data); w > 4096 || h > 4096 {
			// Keep the fuzzer fast: the decode budget is covered separately.
			t.Skip()
		}
		img, err := decodeJPEGScale8Bytes(data, 0)
		if err != nil {
			return
		}
		b := img.Bounds()
		if b.Dx() <= 0 || b.Dy() <= 0 {
			t.Fatalf("fast decoder returned empty bounds %v", b)
		}
		if cfg, cerr := jpeg.DecodeConfig(bytes.NewReader(data)); cerr == nil {
			if b.Dx() != (cfg.Width+7)/8 || b.Dy() != (cfg.Height+7)/8 {
				t.Fatalf("fast decoder bounds %v disagree with stdlib %dx%d", b, cfg.Width, cfg.Height)
			}
		}
		// The result must be encodable through the production thumbnail path.
		if _, err := encodeBoundedJPEG(img, 64); err != nil {
			t.Fatalf("encodeBoundedJPEG: %v", err)
		}
	})
}

// FuzzFindEmbeddedJPEG exercises the RAW-container embedded preview scanner
// (SOI/EOI marker walk and SOF dimension probe).
func FuzzFindEmbeddedJPEG(f *testing.F) {
	for _, s := range fuzzSeedJPEGs(f) {
		f.Add(s, 0)
		f.Add(append([]byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x11, 0x22}, s...), 480)
		f.Add(append(append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'c', 'r', 'x', ' '}, s...), s...), 240)
	}
	f.Fuzz(func(t *testing.T, data []byte, minLongEdge int) {
		best, _ := findBestEmbeddedJPEGForTarget(data, minLongEdge)
		if len(best) > 0 {
			if len(best) > len(data) || best[0] != 0xFF || best[1] != 0xD8 {
				t.Fatalf("candidate is not a JPEG sub-slice of the input (len=%d)", len(best))
			}
		}
		w, h := parseJPEGDimensions(data)
		if w < 0 || h < 0 || w > 0xFFFF || h > 0xFFFF {
			t.Fatalf("parseJPEGDimensions out of range: %dx%d", w, h)
		}
		if end := scanJPEGEnd(data, 0); end > len(data) {
			t.Fatalf("scanJPEGEnd returned %d beyond input length %d", end, len(data))
		}
		_ = HasUltraHDRGainMap(data)
	})
}

func fuzzSeedRasters(tb testing.TB) map[string][]byte {
	tb.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 5), G: uint8(y * 7), B: 90, A: 255})
		}
	}
	out := map[string][]byte{}
	var p, tf bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		tb.Fatal(err)
	}
	out[".png"] = p.Bytes()
	if err := tiff.Encode(&tf, img, &tiff.Options{Compression: tiff.Deflate}); err != nil {
		tb.Fatal(err)
	}
	out[".tif"] = tf.Bytes()
	out[".jpg"] = fuzzSeedJPEGs(tb)[0]
	// Minimal lossless WebP (1x1) and lossy VP8 header shells.
	out[".webp"] = []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")
	// Fujifilm RAF header pointing at an embedded JPEG.
	raf := make([]byte, 160)
	copy(raf, "FUJIFILMCCD-RAW ")
	binary.BigEndian.PutUint32(raf[84:88], 160)
	binary.BigEndian.PutUint32(raf[88:92], uint32(len(out[".jpg"])))
	out[".raf"] = append(raf, out[".jpg"]...)
	out[".dng"] = append([]byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00}, out[".jpg"]...)
	out[".exr"] = []byte{0x76, 0x2F, 0x31, 0x01, 2, 0, 0, 0}
	out[".jxl"] = []byte{0xFF, 0x0A, 0, 0}
	return out
}

// fuzzExts lists one extension per decode route the thumbnail service takes:
// raster decoders, RAW embedded-preview containers, and the HEIC/AVIF/EXR/JXL
// formats that have no pure-Go decoder and must fail cleanly.
var fuzzExts = []string{
	".jpg", ".png", ".webp", ".tif", ".heic", ".avif", ".exr", ".jxl",
	".dng", ".cr2", ".cr3", ".nef", ".arw", ".raf", ".orf", ".rw2",
}

// FuzzThumbnailPipeline drives arbitrary bytes through the full thumbnail
// generation path (header probe, fast/standard decode, embedded preview
// extraction, bounded re-encode) for every supported format family.
func FuzzThumbnailPipeline(f *testing.F) {
	seeds := fuzzSeedRasters(f)
	for i, ext := range fuzzExts {
		if s, ok := seeds[ext]; ok {
			f.Add(s, uint8(i), uint8(0))
			f.Add(s, uint8(i), uint8(2))
		}
	}
	f.Add(seeds[".jpg"], uint8(8), uint8(1))  // JPEG bytes inside a .dng
	f.Add(seeds[".png"], uint8(0), uint8(1))  // PNG bytes named .jpg
	f.Add(seeds[".tif"], uint8(11), uint8(0)) // TIFF bytes named .nef

	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte, extIdx, tierIdx uint8) {
		if len(data) == 0 || len(data) > 1<<20 {
			t.Skip()
		}
		ext := fuzzExts[int(extIdx)%len(fuzzExts)]
		tier := []string{"t0", "t1", "t2"}[int(tierIdx)%3]
		src := filepath.Join(dir, "fuzz"+ext)
		if err := os.WriteFile(src, data, 0o600); err != nil {
			t.Fatal(err)
		}

		w, h, _ := ProbeImageHeader(src)
		if w < 0 || h < 0 {
			t.Fatalf("ProbeImageHeader returned negative dimensions %dx%d", w, h)
		}

		svc := NewService("", 1)
		res, err := svc.generateAndCache(src, "fuzzrev", tier, filepath.Join(dir, "none.jpg"), filepath.Join(dir, "none.src"))
		if err != nil {
			return
		}
		if len(res.Data) < 3 || res.Data[0] != 0xFF || res.Data[1] != 0xD8 {
			t.Fatalf("thumbnail for %s is not a JPEG (%d bytes)", ext, len(res.Data))
		}
		if res.MIME != "image/jpeg" || res.TierSource == "" {
			t.Fatalf("unexpected result metadata: %+v", res)
		}
	})
}
