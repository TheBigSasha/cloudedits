// Package thumb implements the tiered thumbnail pipeline:
//  1. .hdrtoys/<rel>.edit_thumbnail.jpg verbatim (X-HDRT-Tier-Source: edit-thumbnail)
//  2. App-data disk cache (X-HDRT-Tier-Source: embedded|generated)
//  3. Pure-Go embedded JPEG extraction for RAW containers (TIFF/EXIF/CR3/RAF)
//     — selecting the smallest adequate embedded preview, never full RAW decode —
//     or bounded raster downscale (<= 640 px JPEG q82) with single-flight dedup.
package thumb

import (
	"bufio"
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
	"sync"
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

type inflightCall struct {
	done chan struct{}
	res  *Result
	err  error
}

// Service generates and caches thumbnails with single-flight dedup and bounded concurrency.
type Service struct {
	cacheDir string
	sem      chan struct{}
	sfMu     sync.Mutex
	inFlight map[string]*inflightCall
}

// NewService creates a bounded thumbnail service storing cache files under cacheDir.
func NewService(cacheDir string, maxWorkers int) *Service {
	if maxWorkers <= 0 {
		// Cap default concurrent decodes to 3 so cold bursts stay fast while
		// remaining well below the 256 MiB peak RSS budget (and 512 MiB containers).
		maxWorkers = min(3, max(1, runtime.NumCPU()))
	}
	return &Service{
		cacheDir: cacheDir,
		sem:      make(chan struct{}, maxWorkers),
		inFlight: make(map[string]*inflightCall),
	}
}

// Resolve serves a thumbnail according to the normative tier priority:
//  1. editThumbPath (.hdrtoys/<rel>.edit_thumbnail.jpg) if present
//  2. disk cache keyed by (sourceRevision, tier)
//  3. single-flight RAW embedded JPEG preview or bounded raster downscale
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

	// Fast-fail empty or explicitly unsupported formats (e.g. EXR, JXL) before
	// acquiring a decode slot or allocating scan buffers.
	if err := fastRejectUnsupported(sourcePath); err != nil {
		return nil, err
	}

	// Priority 3: single-flight deduplication per (sourcePath, sourceRevision, normTier).
	sfKey := sourcePath + "\x00" + cacheKey
	s.sfMu.Lock()
	if existing, ok := s.inFlight[sfKey]; ok {
		s.sfMu.Unlock()
		select {
		case <-existing.done:
			return existing.res, existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	s.inFlight[sfKey] = call
	s.sfMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				call.res = nil
				call.err = fmt.Errorf("thumbnail panic: %v", r)
			}
			s.sfMu.Lock()
			delete(s.inFlight, sfKey)
			s.sfMu.Unlock()
			close(call.done)
		}()
		call.res, call.err = s.generateAndCache(sourcePath, sourceRevision, normTier, cachePath, metaPath)
	}()

	select {
	case <-call.done:
		return call.res, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) generateAndCache(
	sourcePath, sourceRevision, normTier, cachePath, metaPath string,
) (*Result, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-time.After(5 * time.Second):
		return nil, ErrOverloaded
	}

	// Re-check disk cache in case a predecessor finished right before we acquired the slot.
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

	maxDim := tierMaxDim(normTier)
	var (
		thumbBytes []byte
		tierSource string
		err        error
	)

	if formats.IsRAW(sourcePath) {
		minAdequate := tierMinAdequateRAWDim(normTier)
		thumbBytes, err = ExtractEmbeddedJPEGForTarget(sourcePath, minAdequate)
		if err != nil {
			return nil, fmt.Errorf("extract RAW embedded preview: %w", err)
		}
		tierSource = "embedded"
		// Downscale overly large embedded previews for t0/t1 grid speed, while
		// serving compact adequate camera previews (e.g. 570-1620px <= 512 KiB)
		// verbatim with zero decode/re-encode overhead.
		if normTier == "t0" || len(thumbBytes) > 512*1024 {
			resized, rerr := downscaleJPEGBytes(thumbBytes, maxDim)
			if rerr != nil {
				return nil, fmt.Errorf("decode RAW embedded preview: %w", rerr)
			}
			thumbBytes = resized
		}
	} else {
		// For raster images: downscale into bounded JPEG q82.
		thumbBytes, err = downscaleRasterFile(sourcePath, maxDim)
		if err != nil {
			// Only attempt embedded JPEG extraction for non-JPEG container
			// formats (e.g. HEIC/TIFF with preview). If the source file itself
			// has a JPEG header and failed decode, it is corrupt and must fail
			// with an explicit error (HTTP 422) rather than serving raw bytes.
			if isJPEGFileHeader(sourcePath) {
				return nil, fmt.Errorf("decode JPEG thumbnail: %w", err)
			}
			embedded, eerr := ExtractEmbeddedJPEGForTarget(sourcePath, tierMinAdequateRAWDim(normTier))
			if eerr != nil {
				return nil, fmt.Errorf("generate raster thumbnail: %w", err)
			}
			tierSource = "embedded"
			if normTier == "t0" || len(embedded) > 512*1024 {
				resized, rerr := downscaleJPEGBytes(embedded, maxDim)
				if rerr != nil {
					return nil, fmt.Errorf("decode embedded preview: %w", rerr)
				}
				thumbBytes = resized
			} else {
				thumbBytes = embedded
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

func isJPEGFileHeader(filePath string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var hdr [3]byte
	n, _ := io.ReadFull(f, hdr[:])
	return n >= 3 && formats.IsJPEGMagic(hdr[:n])
}

func fastRejectUnsupported(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return errors.New("empty source file")
	}

	var magic [16]byte
	n, _ := io.ReadFull(f, magic[:])
	if n >= 4 && magic[0] == 0x76 && magic[1] == 0x2F && magic[2] == 0x31 && magic[3] == 0x01 {
		return errors.New("OpenEXR thumbnail generation unsupported")
	}
	if n >= 2 && magic[0] == 0xFF && magic[1] == 0x0A {
		return errors.New("JPEG XL thumbnail generation unsupported")
	}
	if n >= 12 && magic[0] == 0x00 && magic[1] == 0x00 && magic[2] == 0x00 && magic[3] == 0x0C &&
		magic[4] == 'J' && magic[5] == 'X' && magic[6] == 'L' && magic[7] == ' ' {
		return errors.New("JPEG XL thumbnail generation unsupported")
	}
	return nil
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

func tierMinAdequateRAWDim(normTier string) int {
	switch normTier {
	case "t0":
		return 240
	case "t2":
		return 1280
	default:
		return 480
	}
}

// ExtractEmbeddedJPEG extracts the largest embedded JPEG preview from a camera
// RAW or container file using pure-Go header/offset scanning (RAF, CR3/ISOBMFF,
// TIFF/EXIF-based DNG/CR2/NEF/ARW/ORF/RW2/PEF/SRW) without full RAW decoding.
func ExtractEmbeddedJPEG(filePath string) ([]byte, error) {
	return ExtractEmbeddedJPEGForTarget(filePath, 0)
}

// ExtractEmbeddedJPEGForTarget extracts an embedded JPEG preview sized for
// minLongEdge using progressive bounded reads (2 MiB -> 8 MiB -> 12 MiB).
// When minLongEdge > 0, it prefers the smallest valid embedded JPEG whose long
// edge is >= minLongEdge (avoiding decoding 24-50 MP full-res RAW previews when
// a crisp 570-1620px preview exists in the header). When minLongEdge <= 0, it
// returns the largest valid embedded JPEG found.
func ExtractEmbeddedJPEGForTarget(filePath string, minLongEdge int) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fileSize := info.Size()
	if fileSize == 0 {
		return nil, errors.New("empty file")
	}

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

	// Fast reject OpenEXR before reading multi-MiB scan buffers.
	if n >= 4 && hdr[0] == 0x76 && hdr[1] == 0x2F && hdr[2] == 0x31 && hdr[3] == 0x01 {
		return nil, errors.New("no embedded JPEG preview in OpenEXR")
	}

	// Progressive bounded scan windows: most cameras place their 570-1620px
	// preview in the first 125 KB - 1.2 MiB. Only expand if needed.
	windows := []int64{2 * 1024 * 1024, 8 * 1024 * 1024, 12 * 1024 * 1024}
	var fallback []byte

	for i, limit := range windows {
		readLen := min(fileSize, limit)
		if readLen <= 0 {
			break
		}
		buf := make([]byte, readLen)
		gotN, rerr := io.ReadFull(io.NewSectionReader(f, 0, readLen), buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			return nil, rerr
		}
		buf = buf[:gotN]

		cand, adequate := findBestEmbeddedJPEGForTarget(buf, minLongEdge)
		if len(cand) > 0 {
			if adequate || minLongEdge <= 0 || readLen >= fileSize {
				out := make([]byte, len(cand))
				copy(out, cand)
				return out, nil
			}
			// Keep best small candidate so far in case expanding finds nothing better.
			fallback = make([]byte, len(cand))
			copy(fallback, cand)
		}
		if readLen >= fileSize {
			break
		}
		// If the first 2 MiB of a file had zero JPEGs at all and it's a small
		// file or non-container, still check 8 MiB once, but skip 12 MiB if 8 MiB
		// found nothing.
		if i == 1 && len(fallback) == 0 {
			break
		}
	}

	if len(fallback) > 0 {
		return fallback, nil
	}
	return nil, errors.New("no embedded JPEG preview found")
}

// findBestEmbeddedJPEGForTarget scans a byte slice for valid JPEG SOI..EOI
// segments and selects the optimal preview for minLongEdge.
func findBestEmbeddedJPEGForTarget(data []byte, minLongEdge int) (best []byte, adequate bool) {
	var (
		bestAdequate     []byte
		bestAdequateLong int
		bestFallback     []byte
		bestFallbackLong int
	)

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
			w, h := parseJPEGDimensions(candidate)
			longEdge := max(w, h)

			if minLongEdge > 0 {
				if longEdge >= minLongEdge {
					if len(bestAdequate) == 0 ||
						longEdge < bestAdequateLong ||
						(longEdge == bestAdequateLong && len(candidate) < len(bestAdequate)) {
						bestAdequate = candidate
						bestAdequateLong = longEdge
					}
				} else {
					if len(bestFallback) == 0 ||
						longEdge > bestFallbackLong ||
						(longEdge == bestFallbackLong && len(candidate) > len(bestFallback)) {
						bestFallback = candidate
						bestFallbackLong = longEdge
					}
				}
			} else if len(candidate) > len(bestFallback) {
				bestFallback = candidate
				bestFallbackLong = longEdge
			}
			pos = end
			continue
		}
		pos = start + 3
	}

	if len(bestAdequate) > 0 {
		return bestAdequate, true
	}
	if len(bestFallback) > 0 {
		return bestFallback, false
	}
	return nil, false
}

// parseJPEGDimensions reads width and height directly from the JPEG SOF marker
// without decoding any pixels.
func parseJPEGDimensions(data []byte) (width, height int) {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 0, 0
	}
	i := 2
	for i+4 < len(data) {
		if data[i] != 0xFF {
			return 0, 0
		}
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return 0, 0
		}
		marker := data[i]
		i++
		if marker == 0xD8 || marker == 0xD9 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if i+2 > len(data) {
			return 0, 0
		}
		segLen := int(binary.BigEndian.Uint16(data[i : i+2]))
		if segLen < 2 || i+segLen > len(data) {
			return 0, 0
		}
		// SOF0..SOF3 (Baseline, Extended, Progressive, Lossless)
		if marker == 0xC0 || marker == 0xC1 || marker == 0xC2 || marker == 0xC3 {
			if segLen >= 7 && i+7 <= len(data) {
				height = int(binary.BigEndian.Uint16(data[i+3 : i+5]))
				width = int(binary.BigEndian.Uint16(data[i+5 : i+7]))
				return width, height
			}
			return 0, 0
		}
		if marker == 0xDA { // SOS
			return 0, 0
		}
		i += segLen
	}
	return 0, 0
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
	// Fast path: 1/8-scale DC-only decode for large baseline JPEGs when 1/8th
	// resolution is still >= 75% of target maxDim (e.g. >= 3840px for t1=640px).
	minFastDim := (maxDim * 3) / 4
	if img, err := decodeJPEGScale8File(filePath, minFastDim); err == nil {
		return encodeBoundedJPEG(img, maxDim)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	img, _, err := image.Decode(bufio.NewReaderSize(f, 64*1024))
	if err != nil {
		return nil, err
	}
	return encodeBoundedJPEG(img, maxDim)
}

func downscaleJPEGBytes(raw []byte, maxDim int) ([]byte, error) {
	minFastDim := (maxDim * 3) / 4
	if img, err := decodeJPEGScale8Bytes(raw, minFastDim); err == nil {
		return encodeBoundedJPEG(img, maxDim)
	}
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
		// ApproxBiLinear uses specialized YCbCr/RGBA fast paths (~4-5ms on 45 MP
		// vs ~870ms for CatmullRom) with zero intermediate float buffers.
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
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
