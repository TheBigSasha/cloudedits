package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
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
		t.Fatalf("extracted RAF preview mismatch")
	}
}
