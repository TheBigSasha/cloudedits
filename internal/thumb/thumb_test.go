package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func makeSampleJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode sample jpeg: %v", err)
	}
	return buf.Bytes()
}

func TestRasterDownscaleAndEditThumbnailPriority(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	srcPath := filepath.Join(dir, "big.jpg")
	if err := os.WriteFile(srcPath, makeSampleJPEG(t, 1200, 800, color.RGBA{200, 50, 50, 255}), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(cacheDir, 2)
	res, err := svc.Resolve(context.Background(), srcPath, "rev1", "", "t1")
	if err != nil {
		t.Fatalf("Resolve raster: %v", err)
	}
	if res.TierSource != "generated" {
		t.Fatalf("expected generated, got %q", res.TierSource)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(res.Data))
	if err != nil {
		t.Fatalf("decode thumb config: %v", err)
	}
	if cfg.Width != 640 || cfg.Height != 426 {
		t.Fatalf("expected 640x426 aspect-preserved thumb, got %dx%d", cfg.Width, cfg.Height)
	}

	// Edit thumbnail takes highest priority.
	editThumb := filepath.Join(dir, "big.jpg.edit_thumbnail.jpg")
	editBytes := makeSampleJPEG(t, 300, 200, color.RGBA{20, 200, 40, 255})
	if err := os.WriteFile(editThumb, editBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := svc.Resolve(context.Background(), srcPath, "rev1", editThumb, "t1")
	if err != nil {
		t.Fatalf("Resolve edit thumbnail: %v", err)
	}
	if res2.TierSource != "edit-thumbnail" {
		t.Fatalf("expected edit-thumbnail, got %q", res2.TierSource)
	}
	if !bytes.Equal(res2.Data, editBytes) {
		t.Fatalf("expected edit thumbnail bytes verbatim")
	}
}

func TestExtractEmbeddedJPEGFromRAWSynthetics(t *testing.T) {
	dir := t.TempDir()
	preview := makeSampleJPEG(t, 320, 240, color.RGBA{40, 80, 220, 255})

	// 1. TIFF-like RAW (.arw) with embedded JPEG payload after synthetic header.
	arwPath := filepath.Join(dir, "IMG_0001.ARW")
	arwBytes := append([]byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x11, 0x22}, preview...)
	arwBytes = append(arwBytes, []byte{0x00, 0x01, 0x02, 0x03}...)
	if err := os.WriteFile(arwPath, arwBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractEmbeddedJPEG(arwPath)
	if err != nil {
		t.Fatalf("ExtractEmbeddedJPEG(ARW): %v", err)
	}
	if !bytes.Equal(got, preview) {
		t.Fatalf("extracted ARW preview mismatch: got %d bytes want %d", len(got), len(preview))
	}

	// 2. Fujifilm RAF container with offset/length pointers.
	rafPath := filepath.Join(dir, "DSCF0001.RAF")
	var rafBuf bytes.Buffer
	rafBuf.WriteString("FUJIFILMCCD-RAW ")
	rafBuf.Write(make([]byte, 84-16))
	_ = binary.Write(&rafBuf, binary.BigEndian, uint32(128))
	_ = binary.Write(&rafBuf, binary.BigEndian, uint32(len(preview)))
	rafBuf.Write(make([]byte, 128-92))
	rafBuf.Write(preview)
	if err := os.WriteFile(rafPath, rafBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	gotRAF, err := ExtractEmbeddedJPEG(rafPath)
	if err != nil {
		t.Fatalf("ExtractEmbeddedJPEG(RAF): %v", err)
	}
	if !bytes.Equal(gotRAF, preview) {
		t.Fatalf("extracted RAF preview mismatch: got %d bytes want %d", len(gotRAF), len(preview))
	}
}

func TestExtractEmbeddedJPEGForTargetPicksSmallestAdequate(t *testing.T) {
	dir := t.TempDir()
	tiny := makeSampleJPEG(t, 160, 120, color.RGBA{10, 20, 30, 255})
	medium := makeSampleJPEG(t, 800, 600, color.RGBA{40, 120, 200, 255})
	huge := makeSampleJPEG(t, 2400, 1600, color.RGBA{220, 80, 40, 255})

	var raw bytes.Buffer
	raw.Write([]byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00})
	raw.Write(tiny)
	raw.Write([]byte{0x00, 0x11, 0x22, 0x33})
	raw.Write(medium)
	raw.Write([]byte{0x44, 0x55, 0x66, 0x77})
	raw.Write(huge)

	arwPath := filepath.Join(dir, "MULTI.ARW")
	if err := os.WriteFile(arwPath, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	gotT1, err := ExtractEmbeddedJPEGForTarget(arwPath, 480)
	if err != nil {
		t.Fatalf("ExtractEmbeddedJPEGForTarget(480): %v", err)
	}
	if !bytes.Equal(gotT1, medium) {
		t.Fatalf("expected 800x600 medium preview (%d B), got %d B", len(medium), len(gotT1))
	}

	gotT2, err := ExtractEmbeddedJPEGForTarget(arwPath, 1280)
	if err != nil {
		t.Fatalf("ExtractEmbeddedJPEGForTarget(1280): %v", err)
	}
	if !bytes.Equal(gotT2, huge) {
		t.Fatalf("expected 2400x1600 huge preview (%d B), got %d B", len(huge), len(gotT2))
	}

	gotLargest, err := ExtractEmbeddedJPEG(arwPath)
	if err != nil {
		t.Fatalf("ExtractEmbeddedJPEG: %v", err)
	}
	if !bytes.Equal(gotLargest, huge) {
		t.Fatalf("expected largest preview (%d B), got %d B", len(huge), len(gotLargest))
	}
}

func TestFastRejectEmptyAndEXR(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(filepath.Join(dir, "cache"), 2)

	emptyDNG := filepath.Join(dir, "empty.dng")
	if err := os.WriteFile(emptyDNG, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(context.Background(), emptyDNG, "rev-empty", "", "t1"); err == nil {
		t.Fatalf("expected error on 0-byte DNG")
	}

	exrPath := filepath.Join(dir, "scene.exr")
	if err := os.WriteFile(exrPath, []byte{0x76, 0x2F, 0x31, 0x01, 0x02, 0x00, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Resolve(context.Background(), exrPath, "rev-exr", "", "t1"); err == nil {
		t.Fatalf("expected error on OpenEXR")
	}
}

func TestSingleFlightDedupConcurrentResolve(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "burst.jpg")
	if err := os.WriteFile(srcPath, makeSampleJPEG(t, 1280, 854, color.RGBA{90, 140, 210, 255}), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := NewService(filepath.Join(dir, "cache"), 2)
	const n = 12
	results := make([]*Result, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			results[idx], errs[idx] = svc.Resolve(context.Background(), srcPath, "rev-burst", "", "t1")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d failed: %v", i, errs[i])
		}
		if results[i] == nil || len(results[i].Data) == 0 {
			t.Fatalf("goroutine %d empty result", i)
		}
		if !bytes.Equal(results[i].Data, results[0].Data) {
			t.Fatalf("goroutine %d bytes differed from goroutine 0", i)
		}
	}
}

func TestDecodeJPEGScale8MatchesStdlib(t *testing.T) {
	// Gradient RGBA image (1600x1200 -> 1/8 scale = 200x150) verifying color
	// fidelity of the pure-Go SOF0 1/8-scale DC decoder against stdlib jpeg.Decode.
	const w, h = 1600, 1200
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 255) / w),
				G: uint8((y * 255) / h),
				B: uint8(((w - x) * 200) / w),
				A: 255,
			})
		}
	}
	var jpgBuf bytes.Buffer
	if err := jpeg.Encode(&jpgBuf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode gradient jpeg: %v", err)
	}

	fastImg, err := decodeJPEGScale8Bytes(jpgBuf.Bytes(), 150)
	if err != nil {
		t.Fatalf("decodeJPEGScale8Bytes failed: %v", err)
	}
	if fastImg.Bounds().Dx() != w/8 || fastImg.Bounds().Dy() != h/8 {
		t.Fatalf("expected %dx%d, got %dx%d", w/8, h/8, fastImg.Bounds().Dx(), fastImg.Bounds().Dy())
	}

	refImg, err := jpeg.Decode(bytes.NewReader(jpgBuf.Bytes()))
	if err != nil {
		t.Fatalf("stdlib jpeg.Decode: %v", err)
	}

	var totalDiff int64
	var samples int64
	for by := 2; by < h/8-2; by += 5 {
		for bx := 2; bx < w/8-2; bx += 5 {
			r1, g1, b1, _ := fastImg.At(bx, by).RGBA()
			r2, g2, b2, _ := refImg.At(bx*8+4, by*8+4).RGBA()
			dr := int(r1>>8) - int(r2>>8)
			dg := int(g1>>8) - int(g2>>8)
			db := int(b1>>8) - int(b2>>8)
			if dr < 0 {
				dr = -dr
			}
			if dg < 0 {
				dg = -dg
			}
			if db < 0 {
				db = -db
			}
			totalDiff += int64(dr + dg + db)
			samples += 3
		}
	}
	meanDiff := float64(totalDiff) / float64(samples)
	if meanDiff > 4.0 {
		t.Fatalf("1/8-scale DC decode color drift too high: mean channel diff = %.2f (want <= 4.0)", meanDiff)
	}
}

func TestDecodeJPEGScale8RejectsOversubscribedDHTAndZeroDims(t *testing.T) {
	// Malformed JPEG with over-subscribed DHT (bits[0] = 3 -> 3 codes of length 1).
	var overSub bytes.Buffer
	overSub.Write([]byte{0xFF, 0xD8}) // SOI
	// DHT segment: length = 2 + 1 + 16 + 3 = 22
	overSub.Write([]byte{0xFF, 0xC4, 0x00, 22, 0x00})
	overSub.Write([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	overSub.Write([]byte{0x00, 0x01, 0x02})
	// Pad to >= 64 bytes so decodeJPEGScale8Bytes parses DHT.
	overSub.Write(make([]byte, 48))
	overSub.Write([]byte{0xFF, 0xD9})

	if _, err := decodeJPEGScale8Bytes(overSub.Bytes(), 0); !errors.Is(err, errUnsupportedFastJPEG) {
		t.Fatalf("expected errUnsupportedFastJPEG for over-subscribed DHT, got %v", err)
	}

	// Malformed JPEG with SOF0 dimensions = 0x0.
	var zeroDims bytes.Buffer
	zeroDims.Write([]byte{0xFF, 0xD8})
	// SOF0 segment: length = 2 + 6 + 3 = 11 (1 component, height=0, width=0)
	zeroDims.Write([]byte{0xFF, 0xC0, 0x00, 11, 8, 0, 0, 0, 0, 1, 1, 0x11, 0})
	zeroDims.Write(make([]byte, 64))
	zeroDims.Write([]byte{0xFF, 0xD9})

	if _, err := decodeJPEGScale8Bytes(zeroDims.Bytes(), 0); !errors.Is(err, errUnsupportedFastJPEG) {
		t.Fatalf("expected errUnsupportedFastJPEG for 0x0 SOF0, got %v", err)
	}

	// End-to-end Service.Resolve must return an error cleanly without crashing or leaking inFlight.
	dir := t.TempDir()
	badPath := filepath.Join(dir, "corrupt_dht.jpg")
	if err := os.WriteFile(badPath, overSub.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(filepath.Join(dir, "cache"), 2)
	if _, err := svc.Resolve(context.Background(), badPath, "rev-corrupt-dht", "", "t1"); err == nil {
		t.Fatalf("expected Resolve to fail cleanly on corrupt DHT JPEG")
	}
	svc.sfMu.Lock()
	remaining := len(svc.inFlight)
	svc.sfMu.Unlock()
	if remaining != 0 {
		t.Fatalf("expected inFlight map to be empty after failed Resolve, got %d", remaining)
	}

	// Large (>2 MiB) 4096x4096 malformed over-subscribed DHT JPEG must also fail cleanly
	// instead of falling back to serving raw corrupt source bytes.
	var largeCorrupt bytes.Buffer
	largeCorrupt.Write([]byte{0xFF, 0xD8})
	// SOF0: 4096x4096, 1 component, 8-bit
	largeCorrupt.Write([]byte{0xFF, 0xC0, 0x00, 11, 8, 0x10, 0x00, 0x10, 0x00, 1, 1, 0x11, 0})
	// Over-subscribed DHT (bits[0] = 3)
	largeCorrupt.Write([]byte{0xFF, 0xC4, 0x00, 22, 0x00})
	largeCorrupt.Write([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	largeCorrupt.Write([]byte{0x00, 0x01, 0x02})
	// SOS + >2 MiB payload + EOI
	largeCorrupt.Write([]byte{0xFF, 0xDA, 0x00, 8, 1, 1, 0x00, 0, 63, 0})
	largeCorrupt.Write(bytes.Repeat([]byte{0x55}, 2*1024*1024+1024))
	largeCorrupt.Write([]byte{0xFF, 0xD9})

	largePath := filepath.Join(dir, "large_corrupt_dht.jpg")
	if err := os.WriteFile(largePath, largeCorrupt.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := svc.Resolve(context.Background(), largePath, "rev-large-corrupt", "", "t1"); err == nil {
		t.Fatalf("expected error for >2 MiB corrupt DHT JPEG, got HTTP-200-equivalent result (%d bytes, src=%s)", len(res.Data), res.TierSource)
	}
}
