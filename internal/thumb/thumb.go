// Package thumb implements the tiered thumbnail pipeline:
//  1. .hdrtoys/<rel>.edit_thumbnail.jpg verbatim (X-HDRT-Tier-Source: edit-thumbnail)
//  2. App-data disk cache (X-HDRT-Tier-Source: embedded|generated)
//  3. Pure-Go embedded JPEG extraction for RAW containers (TIFF/EXIF/CR3/RAF)
//     — never full RAW decode — or bounded raster downscale (<= 640 px JPEG q82).
package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // Register PNG decoder for raster thumbnail generation.
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff" // Register TIFF decoder for raster thumbnail generation.
	_ "golang.org/x/image/webp" // Register WebP decoder for raster thumbnail generation.

	"github.com/thebigsasha/cloudedits/internal/formats"
)

// ErrOverloaded is returned when the bounded decode pool is saturated.
var ErrOverloaded = errors.New("thumbnail worker pool saturated")

// Result holds the resolved thumbnail bytes and provenance metadata.
type Result struct {
	Data       []byte
	TierSource string // "edit-thumbnail" | "embedded" | "generated"
	ETag       string
	MIME       string
}

// Service generates and caches thumbnails with bounded concurrency.
type Service struct {
	cacheDir string
	sem      chan struct{}
}

// NewService creates a bounded thumbnail service storing cache files under cacheDir.
func NewService(cacheDir string, maxWorkers int) *Service {
	if maxWorkers <= 0 {
		maxWorkers = min(4, max(1, runtime.NumCPU()))
	}
	return &Service{
		cacheDir: cacheDir,
		sem:      make(chan struct{}, maxWorkers),
	}
}

// Resolve serves a thumbnail according to the normative tier priority:
//  1. editThumbPath (.hdrtoys/<rel>.edit_thumbnail.jpg) if present
//  2. disk cache keyed by (sourceRevision, tier)
//  3. RAW embedded JPEG preview or bounded raster downscale
func (s *Service) Resolve(
	ctx context.Context,
	sourcePath string,
	sourceRevision string,
	editThumbPath string,
	tier string,
) (*Result, error) {
	normTier := normalizeTier(tier)

	// Priority 1: edited preview thumbnail (.hdrtoys/<rel>.edit_thumbnail.jpg).
	if editThumbPath != "" {
		if info, err := os.Stat(editThumbPath); err == nil && !info.IsDir() && info.Size() > 0 {
			data, err := os.ReadFile(editThumbPath)
			if err == nil && formats.IsJPEGMagic(data) {
				etag := fmt.Sprintf(`"et-%d-%d"`, info.ModTime().UTC().UnixNano(), info.Size())
				return &Result{
					Data:       data,
					TierSource: "edit-thumbnail",
					ETag:       etag,
					MIME:       "image/jpeg",
				}, nil
			}
		}
	}

	// Priority 2: app-data disk cache.
	cacheKey := sanitizeKey(sourceRevision) + "_" + normTier
	cachePath := filepath.Join(s.cacheDir, cacheKey+".jpg")
	metaPath := filepath.Join(s.cacheDir, cacheKey+".src")
	if data, err := os.ReadFile(cachePath); err == nil && len(data) > 0 {
		src := "generated"
		if b, merr := os.ReadFile(metaPath); merr == nil {
			if trimmed := strings.TrimSpace(string(b)); trimmed != "" {
				src = trimmed
			}
		}
		return &Result{
			Data:       data,
			TierSource: src,
			ETag:       fmt.Sprintf(`"%s-%s"`, sourceRevision, normTier),
			MIME:       "image/jpeg",
		}, nil
	}

	// Priority 3: generate/extract under bounded concurrency semaphore.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(1500 * time.Millisecond):
		return nil, ErrOverloaded
	}

	maxDim := tierMaxDim(normTier)
	var (
		thumbBytes []byte
		tierSource string
		err        error
	)

	if formats.IsRAW(sourcePath) {
		thumbBytes, err = ExtractEmbeddedJPEG(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("extract RAW embedded preview: %w", err)
		}
		tierSource = "embedded"
		// Downscale overly large embedded previews for t0/t1 grid speed.
		if normTier == "t0" || len(thumbBytes) > 512*1024 {
			if resized, rerr := downscaleJPEGBytes(thumbBytes, maxDim); rerr == nil {
				thumbBytes = resized
			}
		}
	} else {
		// For raster images: downscale into bounded JPEG q82.
		thumbBytes, err = downscaleRasterFile(sourcePath, maxDim)
		if err != nil {
			// Fallback: try embedded JPEG scan (e.g. EXR/HEIC/TIFF with embedded JPEG).
			if embedded, eerr := ExtractEmbeddedJPEG(sourcePath); eerr == nil {
				thumbBytes = embedded
				tierSource = "embedded"
			} else {
				return nil, fmt.Errorf("generate raster thumbnail: %w", err)
			}
		} else {
			tierSource = "generated"
		}
	}

	// Best-effort persist into app-data disk cache (never inside photo folders).
	if s.cacheDir != "" {
		if mkErr := os.MkdirAll(s.cacheDir, 0o755); mkErr == nil {
			tmp := cachePath + ".tmp"
			if wErr := os.WriteFile(tmp, thumbBytes, 0o644); wErr == nil {
				_ = os.Rename(tmp, cachePath)
				_ = os.WriteFile(metaPath, []byte(tierSource), 0o644)
			}
		}
	}

	return &Result{
		Data:       thumbBytes,
		TierSource: tierSource,
		ETag:       fmt.Sprintf(`"%s-%s"`, sourceRevision, normTier),
		MIME:       "image/jpeg",
	}, nil
}

// ClearCache removes all cached thumbnails under cacheDir.
func (s *Service) ClearCache() error {
	if s.cacheDir == "" {
		return nil
	}
	return os.RemoveAll(s.cacheDir)
}

func normalizeTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "t0":
		return "t0"
	case "t2", "preview":
		return "t2"
	default:
		return "t1"
	}
}

func tierMaxDim(normTier string) int {
	switch normTier {
	case "t0":
		return 256
	case "t2":
		return 1600
	default:
		return 640
	}
}

// ExtractEmbeddedJPEG extracts the best embedded JPEG preview from a camera RAW
// or container file using pure-Go header/offset scanning (RAF, CR3/ISOBMFF,
// TIFF/EXIF-based DNG/CR2/NEF/ARW/ORF/RW2/PEF/SRW) without full RAW decoding.
func ExtractEmbeddedJPEG(filePath string) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// Fast path 1: Fujifilm RAF header ("FUJIFILMCCD-RAW ").
	// Offset 84: uint32 BE JPEG offset, Offset 88: uint32 BE JPEG length.
	var hdr [128]byte
	n, _ := io.ReadFull(f, hdr[:])
	if n >= 92 && string(hdr[:16]) == "FUJIFILMCCD-RAW " {
		off := int64(binary.BigEndian.Uint32(hdr[84:88]))
		length := int64(binary.BigEndian.Uint32(hdr[88:92]))
		if off > 0 && length > 100 && length <= 32*1024*1024 {
			buf := make([]byte, length)
			if _, rerr := f.ReadAt(buf, off); rerr == nil && formats.IsJPEGMagic(buf) {
				return buf, nil
			}
		}
	}

	// Bounded scan (first 16 MiB covers EXIF/TIFF IFD previews and CR3 PRVW/THMB boxes).
	const maxScanBytes = 16 * 1024 * 1024
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxScanBytes))
	if err != nil {
		return nil, err
	}

	return findBestEmbeddedJPEG(data)
}

// findBestEmbeddedJPEG scans a byte slice for valid JPEG SOI..EOI segments and
// returns the largest valid JPEG segment found.
func findBestEmbeddedJPEG(data []byte) ([]byte, error) {
	var best []byte
	pos := 0
	for pos+4 < len(data) {
		idx := bytes.Index(data[pos:], []byte{0xFF, 0xD8, 0xFF})
		if idx < 0 {
			break
		}
		start := pos + idx
		// Validate 4th byte is a valid JPEG marker (APP0..APP15, DQT, DHT, SOF0..SOF2, COM).
		m := data[start+3]
		if !isPlausibleJPEGStartMarker(m) {
			pos = start + 2
			continue
		}
		end := scanJPEGEnd(data, start)
		if end > start+64 {
			candidate := data[start:end]
			if len(candidate) > len(best) {
				best = candidate
			}
			pos = end
			continue
		}
		pos = start + 3
	}
	if len(best) == 0 {
		return nil, errors.New("no embedded JPEG preview found")
	}
	out := make([]byte, len(best))
	copy(out, best)
	return out, nil
}

func isPlausibleJPEGStartMarker(m byte) bool {
	return (m >= 0xE0 && m <= 0xEF) || m == 0xDB || m == 0xC0 || m == 0xC2 || m == 0xC4 || m == 0xFE
}

// scanJPEGEnd walks JPEG markers from SOI to EOI (FF D9) so embedded thumbnails
// inside APP1/EXIF blocks don't truncate the outer JPEG prematurely.
func scanJPEGEnd(data []byte, start int) int {
	i := start + 2
	for i+1 < len(data) {
		if data[i] != 0xFF {
			return -1
		}
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return -1
		}
		marker := data[i]
		i++

		if marker == 0xD9 { // EOI
			return i
		}
		if marker == 0x00 || (marker >= 0xD0 && marker <= 0xD8) || marker == 0x01 {
			continue
		}
		if i+2 > len(data) {
			return -1
		}
		segLen := int(binary.BigEndian.Uint16(data[i : i+2]))
		if segLen < 2 || i+segLen > len(data) {
			return -1
		}
		i += segLen
		if marker == 0xDA { // SOS -> entropy-coded scan until unescaped FF D9
			for i+1 < len(data) {
				if data[i] == 0xFF {
					next := data[i+1]
					if next == 0x00 || (next >= 0xD0 && next <= 0xD7) {
						i += 2
						continue
					}
					if next == 0xD9 {
						return i + 2
					}
					if next == 0xFF {
						i++
						continue
					}
					// Another marker (e.g. progressive JPEG next SOS/DHT).
					break
				}
				i++
			}
		}
	}
	return -1
}

func downscaleRasterFile(filePath string, maxDim int) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return encodeBoundedJPEG(img, maxDim)
}

func downscaleJPEGBytes(raw []byte, maxDim int) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return encodeBoundedJPEG(img, maxDim)
}

func encodeBoundedJPEG(src image.Image, maxDim int) ([]byte, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("empty image dimensions")
	}

	targetW, targetH := w, h
	if w > maxDim || h > maxDim {
		if w >= h {
			targetW = maxDim
			targetH = max(1, (h*maxDim)/w)
		} else {
			targetH = maxDim
			targetW = max(1, (w*maxDim)/h)
		}
	}

	outImg := src
	if targetW != w || targetH != h {
		dst := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
		outImg = dst
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, outImg, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ProbeImageHeader reads dimensions and UltraHDR gain-map markers from the
// header of a media file without decoding full pixel buffers.
func ProbeImageHeader(filePath string) (width, height int, hasGainMap bool) {
	f, err := os.Open(filePath)
	if err != nil {
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()

	// Read initial 64 KiB for UltraHDR XMP/ISO 21496-1 markers.
	hdr := make([]byte, 64*1024)
	n, _ := f.Read(hdr)
	if n > 0 {
		slice := hdr[:n]
		hasGainMap = HasUltraHDRGainMap(slice)
	}

	if _, err := f.Seek(0, io.SeekStart); err == nil {
		if cfg, _, err := image.DecodeConfig(f); err == nil {
			width = cfg.Width
			height = cfg.Height
		}
	}
	return width, height, hasGainMap
}

// HasUltraHDRGainMap detects UltraHDR / ISO 21496-1 gain-map metadata markers.
func HasUltraHDRGainMap(header []byte) bool {
	return bytes.Contains(header, []byte("hdrgm:Version")) ||
		bytes.Contains(header, []byte("http://ns.adobe.com/hdr-gain-map/1.0/")) ||
		bytes.Contains(header, []byte("urn:iso:std:iso:ts:21496"))
}

func sanitizeKey(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "rev"
	}
	return b.String()
}
