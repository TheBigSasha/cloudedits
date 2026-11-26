package thumb

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildSyntheticTIFF builds a minimal valid TIFF/EXIF block (IFD0 + ExifIFD)
// using byte order bo for deterministic EXIF parser testing.
func buildSyntheticTIFF(bo binary.ByteOrder, withOffset bool) []byte {
	type ifdEntry struct {
		tag   uint16
		typ   uint16
		count uint32
		val   [4]byte
		extra []byte
	}
	makeASCII := func(tag uint16, s string) ifdEntry {
		b := append([]byte(s), 0)
		e := ifdEntry{tag: tag, typ: 2, count: uint32(len(b))}
		if len(b) <= 4 {
			copy(e.val[:], b)
		} else {
			e.extra = b
		}
		return e
	}
	makeShort := func(tag, v uint16) ifdEntry {
		e := ifdEntry{tag: tag, typ: 3, count: 1}
		bo.PutUint16(e.val[0:2], v)
		return e
	}
	makeURat := func(tag uint16, num, den uint32) ifdEntry {
		extra := make([]byte, 8)
		bo.PutUint32(extra[0:4], num)
		bo.PutUint32(extra[4:8], den)
		return ifdEntry{tag: tag, typ: 5, count: 1, extra: extra}
	}
	makeSRat := func(tag uint16, num, den int32) ifdEntry {
		extra := make([]byte, 8)
		bo.PutUint32(extra[0:4], uint32(num))
		bo.PutUint32(extra[4:8], uint32(den))
		return ifdEntry{tag: tag, typ: 10, count: 1, extra: extra}
	}

	exifEntries := []ifdEntry{
		makeURat(0x829A, 1, 250),                 // ExposureTime 1/250
		makeURat(0x829D, 28, 10),                 // FNumber f/2.8
		makeShort(0x8827, 400),                   // ISO 400
		makeASCII(0x9003, "2025:07:04 14:30:00"), // DateTimeOriginal
		makeSRat(0x9204, -1, 3),                  // ExposureBias -0.33
		makeShort(0x9209, 0),                     // Flash off
		makeURat(0x920A, 500, 10),                // FocalLength 50mm
		makeShort(0xA403, 0),                     // WhiteBalance auto
		makeShort(0xA405, 50),                    // FocalLengthIn35mmFilm
		makeASCII(0xA434, "FE 50mm F1.2 GM"),     // LensModel
	}
	if withOffset {
		exifEntries = append(exifEntries, makeASCII(0x9011, "+02:00"))
	}

	// Layout:
	// [0..8): TIFF header
	// [8..ifd0End): IFD0 (4 entries: Make, Model, Orientation, ExifIFDPointer)
	// [ifd0End..exifStart): IFD0 extra data
	// [exifStart..exifEnd): ExifIFD
	// [exifEnd..): ExifIFD extra data
	ifd0Entries := []ifdEntry{
		makeASCII(0x010F, "SONY"),
		makeASCII(0x0110, "ILCE-1"),
		makeShort(0x0112, 6),
		{tag: 0x8769, typ: 4, count: 1}, // ExifIFDPointer patched below
	}

	var buf bytes.Buffer
	if bo == binary.LittleEndian {
		buf.Write([]byte{'I', 'I'})
	} else {
		buf.Write([]byte{'M', 'M'})
	}
	var u16 [2]byte
	var u32 [4]byte
	bo.PutUint16(u16[:], 42)
	buf.Write(u16[:])
	bo.PutUint32(u32[:], 8)
	buf.Write(u32[:])

	ifd0Start := buf.Len()
	ifd0Size := 2 + len(ifd0Entries)*12 + 4
	ifd0ExtraOff := ifd0Start + ifd0Size
	var ifd0Extras bytes.Buffer
	for i := range ifd0Entries {
		if len(ifd0Entries[i].extra) > 0 {
			bo.PutUint32(ifd0Entries[i].val[:], uint32(ifd0ExtraOff+ifd0Extras.Len()))
			ifd0Extras.Write(ifd0Entries[i].extra)
		}
	}
	exifStart := ifd0ExtraOff + ifd0Extras.Len()
	bo.PutUint32(ifd0Entries[3].val[:], uint32(exifStart))

	writeIFD := func(entries []ifdEntry, extras []byte) {
		bo.PutUint16(u16[:], uint16(len(entries)))
		buf.Write(u16[:])
		for _, e := range entries {
			bo.PutUint16(u16[:], e.tag)
			buf.Write(u16[:])
			bo.PutUint16(u16[:], e.typ)
			buf.Write(u16[:])
			bo.PutUint32(u32[:], e.count)
			buf.Write(u32[:])
			buf.Write(e.val[:])
		}
		buf.Write([]byte{0, 0, 0, 0}) // next IFD = 0
		buf.Write(extras)
	}

	writeIFD(ifd0Entries, ifd0Extras.Bytes())

	exifSize := 2 + len(exifEntries)*12 + 4
	exifExtraOff := exifStart + exifSize
	var exifExtras bytes.Buffer
	for i := range exifEntries {
		if len(exifEntries[i].extra) > 0 {
			bo.PutUint32(exifEntries[i].val[:], uint32(exifExtraOff+exifExtras.Len()))
			exifExtras.Write(exifEntries[i].extra)
		}
	}
	writeIFD(exifEntries, exifExtras.Bytes())
	return buf.Bytes()
}

func wrapJPEGWithEXIF(tiff []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8})
	app1 := append([]byte("Exif\x00\x00"), tiff...)
	b.Write([]byte{0xFF, 0xE1})
	var segLen [2]byte
	binary.BigEndian.PutUint16(segLen[:], uint16(len(app1)+2))
	b.Write(segLen[:])
	b.Write(app1)
	b.Write([]byte{0xFF, 0xD9})
	return b.Bytes()
}

func TestParseEXIFBytesJPEGAndTIFF(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    []byte
		wantCap string
	}{
		{"JPEG little-endian with offset", wrapJPEGWithEXIF(buildSyntheticTIFF(binary.LittleEndian, true)), "2025-07-04T12:30:00Z"},
		{"JPEG big-endian UTC fallback", wrapJPEGWithEXIF(buildSyntheticTIFF(binary.BigEndian, false)), "2025-07-04T14:30:00Z"},
		{"TIFF/RAW direct little-endian", buildSyntheticTIFF(binary.LittleEndian, true), "2025-07-04T12:30:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := ParseEXIFBytes(tc.data)
			if meta.Orientation != 6 {
				t.Errorf("Orientation = %d, want 6", meta.Orientation)
			}
			if meta.CapturedAt != tc.wantCap {
				t.Errorf("CapturedAt = %q, want %q", meta.CapturedAt, tc.wantCap)
			}
			for k, want := range map[string]any{
				"make":            "SONY",
				"model":           "ILCE-1",
				"lens":            "FE 50mm F1.2 GM",
				"iso":             400,
				"aperture":        2.8,
				"shutter":         "1/250",
				"focalLength":     50.0,
				"focalLength35mm": 50,
				"whiteBalance":    "auto",
				"flash":           false,
			} {
				if got := meta.Fields[k]; got != want {
					t.Errorf("exif[%q] = %#v, want %#v", k, got, want)
				}
			}
		})
	}
}

func TestProbeEXIFRAFContainer(t *testing.T) {
	jpegWithEXIF := wrapJPEGWithEXIF(buildSyntheticTIFF(binary.BigEndian, true))
	var raf bytes.Buffer
	raf.WriteString("FUJIFILMCCD-RAW ")
	raf.Write(make([]byte, 84-16))
	_ = binary.Write(&raf, binary.BigEndian, uint32(128))
	_ = binary.Write(&raf, binary.BigEndian, uint32(len(jpegWithEXIF)))
	raf.Write(make([]byte, 128-92))
	raf.Write(jpegWithEXIF)

	p := filepath.Join(t.TempDir(), "DSCF0001.RAF")
	if err := os.WriteFile(p, raf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := ProbeEXIF(p)
	if meta.CapturedAt != "2025-07-04T12:30:00Z" || meta.Fields["model"] != "ILCE-1" {
		t.Fatalf("RAF ProbeEXIF = %+v", meta)
	}
}

func FuzzParseEXIF(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0})
	f.Add(buildSyntheticTIFF(binary.LittleEndian, true))
	f.Add(wrapJPEGWithEXIF(buildSyntheticTIFF(binary.BigEndian, false)))
	f.Fuzz(func(t *testing.T, data []byte) {
		meta := ParseEXIFBytes(data)
		if meta.Orientation < 1 || meta.Orientation > 8 {
			t.Fatalf("invalid orientation %d", meta.Orientation)
		}
	})
}
