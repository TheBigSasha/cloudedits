package index

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/config"
	"github.com/thebigsasha/cloudedits/internal/sidecar"
)

func buildEXIFJPEG(t *testing.T, dateTimeOriginal, offset string, gainMap bool) []byte {
	t.Helper()
	var base bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	img.Set(0, 0, color.RGBA{R: 120, G: 80, B: 40, A: 255})
	if err := jpeg.Encode(&base, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	raw := base.Bytes()

	// Build minimal little-endian TIFF with Make, Model, Orientation=6, ExifIFD (DateTimeOriginal, OffsetTimeOriginal, LensModel, FNumber, ExposureTime, FocalLength).
	var tiff bytes.Buffer
	tiff.Write([]byte{'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00})
	// IFD0: 3 entries (Model, Orientation, ExifIFD)
	ifd0Start := 8
	ifd0Size := 2 + 3*12 + 4
	modelBytes := []byte("ILCE-7RM5\x00")
	modelOff := ifd0Start + ifd0Size
	exifStart := modelOff + len(modelBytes)

	writeU16 := func(v uint16) { _ = binary.Write(&tiff, binary.LittleEndian, v) }
	writeU32 := func(v uint32) { _ = binary.Write(&tiff, binary.LittleEndian, v) }

	writeU16(3)
	writeU16(0x0110)
	writeU16(2)
	writeU32(uint32(len(modelBytes)))
	writeU32(uint32(modelOff))
	writeU16(0x0112)
	writeU16(3)
	writeU32(1)
	writeU16(6)
	writeU16(0)
	writeU16(0x8769)
	writeU16(4)
	writeU32(1)
	writeU32(uint32(exifStart))
	writeU32(0)
	tiff.Write(modelBytes)

	// ExifIFD: 4 entries (ExposureTime, DateTimeOriginal, OffsetTimeOriginal, LensModel)
	exifSize := 2 + 4*12 + 4
	expOff := exifStart + exifSize
	dtBytes := append([]byte(dateTimeOriginal), 0)
	dtOff := expOff + 8
	offBytes := append([]byte(offset), 0)
	offOff := dtOff + len(dtBytes)
	lensBytes := []byte("FE 35mm F1.4 GM\x00")
	lensOff := offOff + len(offBytes)

	writeU16(4)
	writeU16(0x829A)
	writeU16(5)
	writeU32(1)
	writeU32(uint32(expOff))
	writeU16(0x9003)
	writeU16(2)
	writeU32(uint32(len(dtBytes)))
	writeU32(uint32(dtOff))
	writeU16(0x9011)
	writeU16(2)
	writeU32(uint32(len(offBytes)))
	writeU32(uint32(offOff))
	writeU16(0xA434)
	writeU16(2)
	writeU32(uint32(len(lensBytes)))
	writeU32(uint32(lensOff))
	writeU32(0)
	writeU32(1)
	writeU32(500)
	tiff.Write(dtBytes)
	tiff.Write(offBytes)
	tiff.Write(lensBytes)

	var out bytes.Buffer
	out.Write(raw[:2]) // SOI
	app1 := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	out.Write([]byte{0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(app1)+2))
	out.Write(app1)
	if gainMap {
		xmp := []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta hdrgm:Version=\"1.0\"/>")
		out.Write([]byte{0xFF, 0xE1})
		_ = binary.Write(&out, binary.BigEndian, uint16(len(xmp)+2))
		out.Write(xmp)
	}
	out.Write(raw[2:])
	return out.Bytes()
}

func TestScannerClassifiesKindsAndExtractsEXIF(t *testing.T) {
	ctx := context.Background()
	photoDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(photoDir, "plain.jpg"), buildEXIFJPEG(t, "2024:08:15 10:00:00", "+00:00", false), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photoDir, "hdr.jpg"), buildEXIFJPEG(t, "2024:08:16 12:00:00", "+02:00", true), 0o644); err != nil {
		t.Fatal(err)
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 8, 8)))
	if err := os.WriteFile(filepath.Join(photoDir, "shot.png"), pngBuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photoDir, "scene.exr"), []byte{0x76, 0x2F, 0x31, 0x01, 0x02, 0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(photoDir, "raw.dng"), []byte{'I', 'I', 0x2A, 0x00, 0x08, 0, 0, 0}, 0o644); err != nil {
		t.Fatal(err)
	}

	folders := []config.FolderConfig{{ID: "p", Path: photoDir, Label: "P"}}
	idx, err := Open(filepath.Join(t.TempDir(), "index.db"), "", folders, sidecar.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idx.Close() }()
	if err := idx.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	wantKinds := map[string]string{
		"plain.jpg": "jpeg",
		"hdr.jpg":   "ultrahdr",
		"shot.png":  "png",
		"scene.exr": "exr",
		"raw.dng":   "raw",
	}
	for name, wantKind := range wantKinds {
		a, err := idx.GetAsset(ctx, MakeAssetID("p", name))
		if err != nil || a == nil {
			t.Fatalf("GetAsset(%s): %v", name, err)
		}
		if a.Kind != wantKind {
			t.Errorf("%s: kind=%q, want %q", name, a.Kind, wantKind)
		}
	}

	plain, _ := idx.GetAsset(ctx, MakeAssetID("p", "plain.jpg"))
	if plain.CapturedAt != "2024-08-15T10:00:00Z" {
		t.Errorf("plain.jpg capturedAt=%q, want 2024-08-15T10:00:00Z", plain.CapturedAt)
	}
	if plain.Orientation != 6 {
		t.Errorf("plain.jpg orientation=%d, want 6", plain.Orientation)
	}
	if plain.EXIF["model"] != "ILCE-7RM5" || plain.EXIF["lens"] != "FE 35mm F1.4 GM" || plain.EXIF["shutter"] != "1/500" {
		t.Errorf("plain.jpg EXIF = %+v", plain.EXIF)
	}

	// Verify backward-compatible and new kind filters.
	for _, tc := range []struct {
		kind string
		want []string
	}{
		{"raw", []string{"raw.dng"}},
		{"image", []string{"hdr.jpg", "plain.jpg", "scene.exr", "shot.png"}},
		{"photo", []string{"hdr.jpg", "plain.jpg", "scene.exr", "shot.png"}},
		{"ultrahdr", []string{"hdr.jpg"}},
		{"jpeg", []string{"hdr.jpg", "plain.jpg"}},
		{"exr", []string{"scene.exr"}},
	} {
		list, _, err := idx.Range(ctx, Filter{Kind: tc.kind, Sort: "name", Dir: "asc"}, 0, 50)
		if err != nil {
			t.Fatalf("kind=%s: %v", tc.kind, err)
		}
		if got := names(list); !equalStrings(got, tc.want) {
			t.Errorf("kind=%s: got %v, want %v", tc.kind, got, tc.want)
		}
	}
}
