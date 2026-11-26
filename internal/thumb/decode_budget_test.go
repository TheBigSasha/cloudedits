package thumb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// pngHeaderOnly returns a PNG whose IHDR declares w x h 8-bit RGBA and whose
// image data is absent: all a decoder needs to allocate the full frame.
func pngHeaderOnly(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8], ihdr[9] = 8, 6
	chunk := func(kind string, data []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(data)))
		b.Write(l[:])
		b.WriteString(kind)
		b.Write(data)
		crc := crc32.NewIEEE()
		crc.Write([]byte(kind))
		crc.Write(data)
		b.Write(crc.Sum(nil))
	}
	chunk("IHDR", ihdr)
	chunk("IDAT", []byte{0x78, 0x01})
	return b.Bytes()
}

// A few dozen bytes of header must not make the thumbnailer allocate
// gigabytes: frames over the decode budget are refused before decoding.
func TestDecodeBudgetRefusesOversizedFrames(t *testing.T) {
	dir := t.TempDir()
	bomb := filepath.Join(dir, "bomb.png")
	if err := os.WriteFile(bomb, pngHeaderOnly(30000, 30000), 0o644); err != nil { // 3.35 GiB RGBA
		t.Fatal(err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := NewService(filepath.Join(dir, "cache"), 1).Resolve(context.Background(), bomb, "rev-bomb", "", "t1")
	runtime.ReadMemStats(&after)

	if !errors.Is(err, ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %v", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Fatalf("refusing a 30000x30000 PNG allocated %d MiB", grew>>20)
	}

	for _, tc := range []struct {
		name        string
		cfg         image.Config
		progressive bool
		wantErr     bool
	}{
		{"45 MP camera JPEG (128 MiB YCbCr <= 192 MiB)", image.Config{ColorModel: color.YCbCrModel, Width: 8192, Height: 5464}, false, false},
		{"12 MP RGBA PNG (45.8 MiB <= 192 MiB)", image.Config{ColorModel: color.NRGBAModel, Width: 4000, Height: 3000}, false, false},
		{"10 MP progressive JPEG (148 MiB <= 192 MiB)", image.Config{ColorModel: color.YCbCrModel, Width: 4000, Height: 2600}, true, false},
		{"100 MP full-decode YCbCr (> 192 MiB budget)", image.Config{ColorModel: color.YCbCrModel, Width: 11648, Height: 8736}, false, true},
		{"100 MP 16-bit TIFF (776 MiB > 192 MiB budget)", image.Config{ColorModel: color.RGBA64Model, Width: 11648, Height: 8736}, false, true},
		{"45 MP progressive JPEG (640 MiB > 192 MiB budget)", image.Config{ColorModel: color.YCbCrModel, Width: 8192, Height: 5464}, true, true},
		{"max lossless WebP (1024 MiB > 192 MiB budget)", image.Config{ColorModel: color.NRGBAModel, Width: 16383, Height: 16383}, false, true},
		{"gigapixel PNG", image.Config{ColorModel: color.NRGBAModel, Width: 40000, Height: 40000}, false, true},
		{"max-dimension JPEG", image.Config{ColorModel: color.YCbCrModel, Width: 65535, Height: 65535}, false, true},
		{"int overflow dimensions", image.Config{ColorModel: color.RGBA64Model, Width: 1 << 31, Height: 1 << 31}, false, true},
		{"zero width", image.Config{ColorModel: color.GrayModel, Width: 0, Height: 10}, false, true},
	} {
		release, err := acquireDecodeBudget(tc.cfg, tc.progressive)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
		if release != nil {
			release()
		}
	}
}

func TestIsProgressiveJPEGUsesFrameHeader(t *testing.T) {
	probe := func(b []byte) bool { return isProgressiveJPEG(bufio.NewReader(bytes.NewReader(b))) }
	baseline := makeSampleJPEG(t, 96, 64, color.RGBA{R: 200, G: 50, B: 50, A: 255})
	if probe(baseline) {
		t.Fatal("baseline JPEG reported as progressive")
	}
	if !probe(bytes.Replace(baseline, []byte{0xFF, 0xC0}, []byte{0xFF, 0xC2}, 1)) {
		t.Fatal("SOF2 JPEG not reported as progressive")
	}
	for _, junk := range [][]byte{nil, {0xFF}, {0xFF, 0xD8}, {0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x01}, bytes.Repeat([]byte{0xFF}, 4096)} {
		if probe(junk) {
			t.Fatalf("junk %x reported as progressive", junk)
		}
	}
}
