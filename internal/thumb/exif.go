package thumb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
)

const (
	maxEXIFHeaderReadBytes = 256 * 1024
	maxEXIFIFDEntries      = 128
	maxEXIFStringBytes     = 256
)

// EXIFMeta holds lightweight, privacy-safe EXIF metadata extracted directly
// from image or camera-RAW headers without decoding pixels.
type EXIFMeta struct {
	Orientation int
	CapturedAt  string
	Fields      map[string]any
}

// ProbeEXIF reads header-only EXIF metadata from JPEG, TIFF/RAW (DNG, CR2,
// NEF, ARW, ORF, RW2, PEF, SRW), or Fujifilm RAF containers. Never decodes
// pixels and never extracts GPS coordinates.
func ProbeEXIF(filePath string) (meta EXIFMeta) {
	meta.Orientation = 1
	f, err := os.Open(filePath)
	if err != nil {
		return meta
	}
	defer func() { _ = f.Close() }()
	defer func() {
		if r := recover(); r != nil {
			meta = EXIFMeta{Orientation: 1}
		}
	}()

	buf := make([]byte, maxEXIFHeaderReadBytes)
	n, _ := io.ReadFull(f, buf)
	if n < 12 {
		return meta
	}
	hdr := buf[:n]

	// Fujifilm RAF: follow the embedded JPEG header offset when present.
	if n >= 92 && string(hdr[:16]) == "FUJIFILMCCD-RAW " {
		off := int64(binary.BigEndian.Uint32(hdr[84:88]))
		length := int64(binary.BigEndian.Uint32(hdr[88:92]))
		if off > 0 && length > 16 && length <= 32*1024*1024 {
			readLen := min(length, int64(maxEXIFHeaderReadBytes))
			jbuf := make([]byte, readLen)
			jn, _ := f.ReadAt(jbuf, off)
			if jn >= 12 {
				return ParseEXIFBytes(jbuf[:jn])
			}
		}
	}

	return ParseEXIFBytes(hdr)
}

// ParseEXIFBytes extracts EXIF metadata from an in-memory JPEG or TIFF header
// prefix. Safe against arbitrary malformed or truncated input.
func ParseEXIFBytes(data []byte) EXIFMeta {
	meta := EXIFMeta{Orientation: 1}
	tiff := locateTIFFHeader(data)
	if len(tiff) < 8 {
		return meta
	}

	var bo binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		bo = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	default:
		return meta
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return meta
	}
	ifd0Off := int(bo.Uint32(tiff[4:8]))
	if ifd0Off < 8 || ifd0Off >= len(tiff) {
		return meta
	}

	fields := map[string]any{}
	var (
		exifIFDOff      int
		dtOriginal      string
		dtDigitized     string
		dtModify        string
		offsetOriginal  string
		offsetDigitized string
		offsetModify    string
	)

	walkIFD(tiff, bo, ifd0Off, func(tag, typ uint16, count uint32, valOff int) {
		switch tag {
		case 0x010F: // Make
			if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
				fields["make"] = s
			}
		case 0x0110: // Model
			if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
				fields["model"] = s
			}
		case 0x0112: // Orientation
			if v, ok := readEXIFUint(tiff, bo, typ, count, valOff); ok && v >= 1 && v <= 8 {
				meta.Orientation = int(v)
			}
		case 0x0132: // DateTime
			if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
				dtModify = s
			}
		case 0x8769: // ExifIFDPointer
			if v, ok := readEXIFUint(tiff, bo, typ, count, valOff); ok && v >= 8 && int(v) < len(tiff) {
				exifIFDOff = int(v)
			}
		}
	})

	if exifIFDOff > 0 && exifIFDOff != ifd0Off {
		walkIFD(tiff, bo, exifIFDOff, func(tag, typ uint16, count uint32, valOff int) {
			switch tag {
			case 0x829A: // ExposureTime
				if num, den, ok := readEXIFURational(tiff, bo, typ, count, valOff); ok && den > 0 && num > 0 {
					sec := float64(num) / float64(den)
					fields["exposureTime"] = roundFloat(sec, 6)
					fields["shutter"] = formatShutter(num, den)
				}
			case 0x829D: // FNumber
				if num, den, ok := readEXIFURational(tiff, bo, typ, count, valOff); ok && den > 0 && num > 0 {
					fields["aperture"] = roundFloat(float64(num)/float64(den), 2)
				}
			case 0x8827: // ISOSpeedRatings / PhotographicSensitivity
				if v, ok := readEXIFUint(tiff, bo, typ, count, valOff); ok && v > 0 {
					fields["iso"] = int(v)
				}
			case 0x9003: // DateTimeOriginal
				if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
					dtOriginal = s
				}
			case 0x9004: // DateTimeDigitized
				if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
					dtDigitized = s
				}
			case 0x9010: // OffsetTime
				if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
					offsetModify = s
				}
			case 0x9011: // OffsetTimeOriginal
				if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
					offsetOriginal = s
				}
			case 0x9012: // OffsetTimeDigitized
				if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
					offsetDigitized = s
				}
			case 0x9204: // ExposureBiasValue
				if num, den, ok := readEXIFSRational(tiff, bo, typ, count, valOff); ok && den != 0 {
					fields["exposureBias"] = roundFloat(float64(num)/float64(den), 2)
				}
			case 0x9209: // Flash
				if v, ok := readEXIFUint(tiff, bo, typ, count, valOff); ok {
					fields["flash"] = (v & 1) != 0
				}
			case 0x920A: // FocalLength
				if num, den, ok := readEXIFURational(tiff, bo, typ, count, valOff); ok && den > 0 && num > 0 {
					fields["focalLength"] = roundFloat(float64(num)/float64(den), 2)
				}
			case 0xA403: // WhiteBalance
				if v, ok := readEXIFUint(tiff, bo, typ, count, valOff); ok {
					switch v {
					case 0:
						fields["whiteBalance"] = "auto"
					case 1:
						fields["whiteBalance"] = "manual"
					}
				}
			case 0xA405: // FocalLengthIn35mmFilm
				if v, ok := readEXIFUint(tiff, bo, typ, count, valOff); ok && v > 0 {
					fields["focalLength35mm"] = int(v)
				}
			case 0xA434: // LensModel
				if s := readEXIFASCII(tiff, typ, count, valOff); s != "" {
					fields["lens"] = s
				}
			}
		})
	}

	rawDT, rawOff := dtOriginal, offsetOriginal
	if rawDT == "" {
		rawDT, rawOff = dtDigitized, offsetDigitized
	}
	if rawDT == "" {
		rawDT, rawOff = dtModify, offsetModify
	}
	if rawDT != "" {
		fields["dateTimeOriginal"] = rawDT
		if ts, ok := parseEXIFTimestamp(rawDT, rawOff); ok {
			meta.CapturedAt = ts
		}
	}
	if len(fields) > 0 {
		meta.Fields = fields
	}
	return meta
}

func locateTIFFHeader(data []byte) []byte {
	if len(data) < 8 {
		return nil
	}
	// Direct TIFF / RAW container (DNG, CR2, NEF, ARW, ORF, RW2, PEF, SRW, TIFF).
	if (data[0] == 'I' && data[1] == 'I' && data[2] == 0x2A && data[3] == 0x00) ||
		(data[0] == 'M' && data[1] == 'M' && data[2] == 0x00 && data[3] == 0x2A) {
		return data
	}
	// JPEG: scan segments before SOS for APP1 "Exif\x00\x00".
	if data[0] != 0xFF || data[1] != 0xD8 {
		return nil
	}
	i := 2
	for segments := 0; segments < 256 && i+4 <= len(data); segments++ {
		if data[i] != 0xFF {
			return nil
		}
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return nil
		}
		marker := data[i]
		i++
		if marker == 0xDA || marker == 0xD9 {
			return nil
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8) {
			continue
		}
		if i+2 > len(data) {
			return nil
		}
		segLen := int(binary.BigEndian.Uint16(data[i : i+2]))
		if segLen < 2 || i+segLen > len(data) {
			return nil
		}
		segBody := data[i+2 : i+segLen]
		if marker == 0xE1 && bytes.HasPrefix(segBody, []byte("Exif\x00\x00")) {
			return segBody[6:]
		}
		i += segLen
	}
	return nil
}

func walkIFD(tiff []byte, bo binary.ByteOrder, off int, fn func(tag, typ uint16, count uint32, valOff int)) {
	if off < 0 || off+2 > len(tiff) {
		return
	}
	n := min(int(bo.Uint16(tiff[off:off+2])), maxEXIFIFDEntries)
	pos := off + 2
	for i := 0; i < n; i++ {
		if pos+12 > len(tiff) {
			return
		}
		tag := bo.Uint16(tiff[pos : pos+2])
		typ := bo.Uint16(tiff[pos+2 : pos+4])
		count := bo.Uint32(tiff[pos+4 : pos+8])
		fn(tag, typ, count, pos+8)
		pos += 12
	}
}

func readEXIFASCII(tiff []byte, typ uint16, count uint32, valOff int) string {
	if typ != 2 || count == 0 || count > maxEXIFStringBytes {
		return ""
	}
	var raw []byte
	if count <= 4 {
		if valOff+int(count) > len(tiff) {
			return ""
		}
		raw = tiff[valOff : valOff+int(count)]
	} else {
		if valOff+4 > len(tiff) {
			return ""
		}
		var bo binary.ByteOrder = binary.BigEndian
		if tiff[0] == 'I' {
			bo = binary.LittleEndian
		}
		off := int(bo.Uint32(tiff[valOff : valOff+4]))
		if off < 0 || off+int(count) > len(tiff) {
			return ""
		}
		raw = tiff[off : off+int(count)]
	}
	if idx := bytes.IndexByte(raw, 0); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(string(raw))
}

func readEXIFUint(tiff []byte, bo binary.ByteOrder, typ uint16, count uint32, valOff int) (uint32, bool) {
	if count == 0 || valOff+4 > len(tiff) {
		return 0, false
	}
	switch typ {
	case 3: // SHORT
		if count == 1 {
			return uint32(bo.Uint16(tiff[valOff : valOff+2])), true
		}
		if count == 2 {
			return uint32(bo.Uint16(tiff[valOff : valOff+2])), true
		}
		off := int(bo.Uint32(tiff[valOff : valOff+4]))
		if off >= 0 && off+2 <= len(tiff) {
			return uint32(bo.Uint16(tiff[off : off+2])), true
		}
	case 4: // LONG
		if count == 1 {
			return bo.Uint32(tiff[valOff : valOff+4]), true
		}
		off := int(bo.Uint32(tiff[valOff : valOff+4]))
		if off >= 0 && off+4 <= len(tiff) {
			return bo.Uint32(tiff[off : off+4]), true
		}
	}
	return 0, false
}

func readEXIFURational(tiff []byte, bo binary.ByteOrder, typ uint16, count uint32, valOff int) (uint32, uint32, bool) {
	if typ != 5 || count == 0 || valOff+4 > len(tiff) {
		return 0, 0, false
	}
	off := int(bo.Uint32(tiff[valOff : valOff+4]))
	if off < 0 || off+8 > len(tiff) {
		return 0, 0, false
	}
	return bo.Uint32(tiff[off : off+4]), bo.Uint32(tiff[off+4 : off+8]), true
}

func readEXIFSRational(tiff []byte, bo binary.ByteOrder, typ uint16, count uint32, valOff int) (int32, int32, bool) {
	if typ != 10 || count == 0 || valOff+4 > len(tiff) {
		return 0, 0, false
	}
	off := int(bo.Uint32(tiff[valOff : valOff+4]))
	if off < 0 || off+8 > len(tiff) {
		return 0, 0, false
	}
	return int32(bo.Uint32(tiff[off : off+4])), int32(bo.Uint32(tiff[off+4 : off+8])), true
}

func formatShutter(num, den uint32) string {
	if num == 0 || den == 0 {
		return ""
	}
	if num >= den {
		sec := float64(num) / float64(den)
		if sec == math.Trunc(sec) {
			return fmt.Sprintf("%ds", int64(sec))
		}
		return fmt.Sprintf("%.1fs", sec)
	}
	denom := math.Round(float64(den) / float64(num))
	if denom < 1 {
		denom = 1
	}
	return fmt.Sprintf("1/%d", int64(denom))
}

func roundFloat(v float64, decimals int) float64 {
	p := math.Pow(10, float64(decimals))
	return math.Round(v*p) / p
}

func parseEXIFTimestamp(dt, offset string) (string, bool) {
	// Standard EXIF format: "YYYY:MM:DD HH:MM:SS"
	dt = strings.TrimSpace(dt)
	if len(dt) < 19 {
		return "", false
	}
	if strings.HasPrefix(dt, "0000:00:00") {
		return "", false
	}
	offset = strings.TrimSpace(offset)
	if offset != "" {
		if t, err := time.Parse("2006:01:02 15:04:05-07:00", dt[:19]+offset); err == nil {
			return t.UTC().Format(time.RFC3339), true
		}
		if offset == "Z" {
			if t, err := time.Parse("2006:01:02 15:04:05", dt[:19]); err == nil {
				return t.UTC().Format(time.RFC3339), true
			}
		}
	}
	t, err := time.Parse("2006:01:02 15:04:05", dt[:19])
	if err != nil {
		return "", false
	}
	return t.UTC().Format(time.RFC3339), true
}
