package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"os"
)

var errUnsupportedFastJPEG = errors.New("unsupported for fast 1/8 JPEG decode")

const huffLUTBits = 8

type huffTable struct {
	valid bool
	// Fast 8-bit lookup: low 8 bits = symbol, high 8 bits = code length (0 = slow path).
	lut      [1 << huffLUTBits]uint16
	minCode  [17]int32
	maxCode  [17]int32
	valIndex [17]int32
	values   [256]uint8
}

func (h *huffTable) build(bits []byte, vals []byte) error {
	if len(bits) != 16 {
		return errUnsupportedFastJPEG
	}
	total := 0
	for _, c := range bits {
		total += int(c)
	}
	if total > 256 || len(vals) < total {
		return errUnsupportedFastJPEG
	}
	copy(h.values[:total], vals[:total])
	for i := range h.lut {
		h.lut[i] = 0
	}

	var code int32
	idx := int32(0)
	for length := 1; length <= 16; length++ {
		count := int32(bits[length-1])
		if count == 0 {
			h.maxCode[length] = -1
			h.minCode[length] = 0
			h.valIndex[length] = 0
		} else {
			h.minCode[length] = code
			h.valIndex[length] = idx
			h.maxCode[length] = code + count - 1
			if length <= huffLUTBits {
				shift := huffLUTBits - length
				for j := int32(0); j < count; j++ {
					sym := uint16(h.values[idx+j])
					entry := (uint16(length) << 8) | sym
					base := int((code + j) << shift)
					span := 1 << shift
					for k := 0; k < span; k++ {
						h.lut[base+k] = entry
					}
				}
			}
			code += count
			idx += count
		}
		code <<= 1
	}
	h.valid = true
	return nil
}

type bitReader struct {
	data   []byte
	pos    int
	buf    uint64
	nBits  int
	hitEOF bool
}

func (br *bitReader) refill() {
	for br.nBits <= 56 {
		if br.pos >= len(br.data) {
			br.hitEOF = true
			return
		}
		b := br.data[br.pos]
		br.pos++
		if b == 0xFF {
			// Skip fill 0xFF bytes.
			for br.pos < len(br.data) && br.data[br.pos] == 0xFF {
				br.pos++
			}
			if br.pos >= len(br.data) {
				br.hitEOF = true
				return
			}
			next := br.data[br.pos]
			if next == 0x00 {
				// Stuffed literal 0xFF byte.
				br.pos++
			} else {
				// Reached a real marker (RSTm or EOI): back up to 0xFF and stop filling.
				br.pos -= 2
				return
			}
		}
		br.buf = (br.buf << 8) | uint64(b)
		br.nBits += 8
	}
}

func (br *bitReader) decodeHuff(ht *huffTable) (uint8, error) {
	if br.nBits < huffLUTBits {
		br.refill()
	}
	if br.nBits >= huffLUTBits {
		top := uint8(br.buf >> (br.nBits - huffLUTBits))
		if entry := ht.lut[top]; entry != 0 {
			length := int(entry >> 8)
			br.nBits -= length
			return uint8(entry & 0xFF), nil
		}
	}

	var code int32
	for length := 1; length <= 16; length++ {
		if br.nBits == 0 {
			br.refill()
			if br.nBits == 0 {
				return 0, io.ErrUnexpectedEOF
			}
		}
		br.nBits--
		bit := int32((br.buf >> br.nBits) & 1)
		code = (code << 1) | bit
		if ht.maxCode[length] >= 0 && code <= ht.maxCode[length] {
			idx := ht.valIndex[length] + (code - ht.minCode[length])
			if idx < 0 || int(idx) >= len(ht.values) {
				return 0, errUnsupportedFastJPEG
			}
			return ht.values[idx], nil
		}
	}
	return 0, errUnsupportedFastJPEG
}

func (br *bitReader) receiveExtend(n int) (int, error) {
	if n == 0 {
		return 0, nil
	}
	if br.nBits < n {
		br.refill()
		if br.nBits < n {
			return 0, io.ErrUnexpectedEOF
		}
	}
	br.nBits -= n
	v := int((br.buf >> br.nBits) & ((uint64(1) << n) - 1))
	if v < (1 << (n - 1)) {
		v -= (1 << n) - 1
	}
	return v, nil
}

func (br *bitReader) skipBits(n int) error {
	if n == 0 {
		return nil
	}
	if br.nBits < n {
		br.refill()
		if br.nBits < n {
			return io.ErrUnexpectedEOF
		}
	}
	br.nBits -= n
	return nil
}

func (br *bitReader) consumeRestartMarker(expectedRST byte) error {
	// Discard remaining buffered bits (byte-aligned before restart marker).
	br.nBits = 0
	br.buf = 0
	for br.pos < len(br.data) && br.data[br.pos] != 0xFF {
		br.pos++
	}
	for br.pos < len(br.data) && br.data[br.pos] == 0xFF {
		br.pos++
	}
	if br.pos >= len(br.data) {
		return io.ErrUnexpectedEOF
	}
	m := br.data[br.pos]
	if m != expectedRST {
		return errUnsupportedFastJPEG
	}
	br.pos++
	return nil
}

type sofComp struct {
	id   byte
	h    int
	v    int
	qtID int
	dcHT int
	acHT int
}

// decodeJPEGScale8Bytes performs a pure-Go 1/8-scale DC-only decode of a
// Baseline Sequential (SOF0) JPEG when its 1/8th-scale dimensions are at least
// minResultLongEdge. Every 8x8 MCU block's DC coefficient is the exact 8x8
// box-average of its 64 pixels, cutting memory by ~64x and decode time by ~6-8x
// on 12-50 MP camera JPEGs. Unsupported JPEGs return errUnsupportedFastJPEG so
// callers fall back seamlessly to standard image.Decode.
func decodeJPEGScale8Bytes(data []byte, minResultLongEdge int) (image.Image, error) {
	if len(data) < 64 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, errUnsupportedFastJPEG
	}

	var (
		qtDC       [4]int
		qtValid    [4]bool
		dcTables   [4]huffTable
		acTables   [4]huffTable
		comps      [3]sofComp
		nComp      int
		width      int
		height     int
		gotSOF0    bool
		restartInt int
		adobeRGB   bool
	)

	i := 2
	sosStart := -1
	for i+2 <= len(data) {
		if data[i] != 0xFF {
			return nil, errUnsupportedFastJPEG
		}
		for i < len(data) && data[i] == 0xFF {
			i++
		}
		if i >= len(data) {
			return nil, errUnsupportedFastJPEG
		}
		marker := data[i]
		i++

		if marker == 0xD9 { // EOI before SOS
			return nil, errUnsupportedFastJPEG
		}
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}
		if i+2 > len(data) {
			return nil, errUnsupportedFastJPEG
		}
		segLen := int(binary.BigEndian.Uint16(data[i : i+2]))
		if segLen < 2 || i+segLen > len(data) {
			return nil, errUnsupportedFastJPEG
		}
		seg := data[i+2 : i+segLen]
		i += segLen

		switch marker {
		case 0xDB: // DQT
			p := 0
			for p < len(seg) {
				pq := seg[p] >> 4
				tq := int(seg[p] & 0x0F)
				p++
				if tq >= 4 {
					return nil, errUnsupportedFastJPEG
				}
				switch pq {
				case 0:
					if p+64 > len(seg) {
						return nil, errUnsupportedFastJPEG
					}
					qtDC[tq] = int(seg[p])
					qtValid[tq] = true
					p += 64
				case 1:
					if p+128 > len(seg) {
						return nil, errUnsupportedFastJPEG
					}
					qtDC[tq] = int(binary.BigEndian.Uint16(seg[p : p+2]))
					qtValid[tq] = true
					p += 128
				default:
					return nil, errUnsupportedFastJPEG
				}
			}

		case 0xC4: // DHT
			p := 0
			for p < len(seg) {
				tc := seg[p] >> 4
				th := int(seg[p] & 0x0F)
				p++
				if tc > 1 || th >= 4 || p+16 > len(seg) {
					return nil, errUnsupportedFastJPEG
				}
				bits := seg[p : p+16]
				p += 16
				sum := 0
				for _, b := range bits {
					sum += int(b)
				}
				if p+sum > len(seg) {
					return nil, errUnsupportedFastJPEG
				}
				vals := seg[p : p+sum]
				p += sum
				if tc == 0 {
					if err := dcTables[th].build(bits, vals); err != nil {
						return nil, err
					}
				} else {
					if err := acTables[th].build(bits, vals); err != nil {
						return nil, err
					}
				}
			}

		case 0xDD: // DRI
			if len(seg) != 2 {
				return nil, errUnsupportedFastJPEG
			}
			restartInt = int(binary.BigEndian.Uint16(seg))

		case 0xEE: // APP14 Adobe
			if len(seg) >= 12 && bytes.Equal(seg[:5], []byte("Adobe")) {
				if seg[11] == 0 {
					adobeRGB = true
				}
			}

		case 0xC0: // SOF0 Baseline Sequential DCT
			if len(seg) < 6 {
				return nil, errUnsupportedFastJPEG
			}
			prec := seg[0]
			if prec != 8 {
				return nil, errUnsupportedFastJPEG
			}
			height = int(binary.BigEndian.Uint16(seg[1:3]))
			width = int(binary.BigEndian.Uint16(seg[3:5]))
			nComp = int(seg[5])
			if (nComp != 1 && nComp != 3) || len(seg) != 6+3*nComp {
				return nil, errUnsupportedFastJPEG
			}
			// Bail early if image isn't large enough for 1/8th-scale decode.
			bw := (width + 7) / 8
			bh := (height + 7) / 8
			if max(bw, bh) < minResultLongEdge {
				return nil, errUnsupportedFastJPEG
			}
			for c := 0; c < nComp; c++ {
				id := seg[6+3*c]
				hv := seg[6+3*c+1]
				tq := int(seg[6+3*c+2])
				if tq >= 4 {
					return nil, errUnsupportedFastJPEG
				}
				comps[c] = sofComp{
					id:   id,
					h:    int(hv >> 4),
					v:    int(hv & 0x0F),
					qtID: tq,
				}
			}
			gotSOF0 = true

		case 0xC1, 0xC2, 0xC3: // Extended / Progressive / Lossless
			return nil, errUnsupportedFastJPEG

		case 0xDA: // SOS
			if !gotSOF0 || adobeRGB {
				return nil, errUnsupportedFastJPEG
			}
			if len(seg) < 1 {
				return nil, errUnsupportedFastJPEG
			}
			ns := int(seg[0])
			if ns != nComp || len(seg) != 1+2*ns+3 {
				return nil, errUnsupportedFastJPEG
			}
			// Reject RGB component IDs ('R', 'G', 'B').
			if nComp == 3 && comps[0].id == 'R' && comps[1].id == 'G' && comps[2].id == 'B' {
				return nil, errUnsupportedFastJPEG
			}
			// Match scan components in order.
			scanOrder := [3]int{}
			for s := 0; s < ns; s++ {
				cs := seg[1+2*s]
				tdta := seg[1+2*s+1]
				td := int(tdta >> 4)
				ta := int(tdta & 0x0F)
				if td >= 4 || ta >= 4 || !dcTables[td].valid || !acTables[ta].valid {
					return nil, errUnsupportedFastJPEG
				}
				found := -1
				for c := 0; c < nComp; c++ {
					if comps[c].id == cs {
						found = c
						break
					}
				}
				if found < 0 {
					return nil, errUnsupportedFastJPEG
				}
				comps[found].dcHT = td
				comps[found].acHT = ta
				scanOrder[s] = found
			}
			// Must be full baseline spectral selection (Ss=0, Se=63, Ah=0, Al=0).
			if seg[1+2*ns] != 0 || seg[1+2*ns+1] != 63 || seg[1+2*ns+2] != 0 {
				return nil, errUnsupportedFastJPEG
			}
			// Require standard component order (Y, Cb, Cr) in scan.
			for s := 0; s < ns; s++ {
				if scanOrder[s] != s {
					return nil, errUnsupportedFastJPEG
				}
				if !qtValid[comps[s].qtID] {
					return nil, errUnsupportedFastJPEG
				}
			}
			sosStart = i
		}

		if sosStart >= 0 {
			break
		}
	}

	if sosStart < 0 || !gotSOF0 {
		return nil, errUnsupportedFastJPEG
	}

	bw := (width + 7) / 8
	bh := (height + 7) / 8

	br := bitReader{data: data, pos: sosStart}

	if nComp == 1 {
		if comps[0].h != 1 || comps[0].v != 1 {
			return nil, errUnsupportedFastJPEG
		}
		gray := image.NewGray(image.Rect(0, 0, bw, bh))
		dcHT := &dcTables[comps[0].dcHT]
		acHT := &acTables[comps[0].acHT]
		q0 := qtDC[comps[0].qtID]
		dcPred := 0
		mcuCount := 0
		rstIdx := byte(0)

		for by := 0; by < bh; by++ {
			rowOff := by * gray.Stride
			for bx := 0; bx < bw; bx++ {
				if restartInt > 0 && mcuCount > 0 && mcuCount%restartInt == 0 {
					if err := br.consumeRestartMarker(0xD0 + (rstIdx & 7)); err != nil {
						return nil, err
					}
					rstIdx++
					dcPred = 0
				}
				dc, err := decodeDCBlock(&br, dcHT, acHT, &dcPred)
				if err != nil {
					return nil, err
				}
				gray.Pix[rowOff+bx] = clampDCByte(dc, q0)
				mcuCount++
			}
		}
		return gray, nil
	}

	// 3-component YCbCr: validate standard subsampling factors.
	h0, v0 := comps[0].h, comps[0].v
	if comps[1].h != 1 || comps[1].v != 1 || comps[2].h != 1 || comps[2].v != 1 {
		return nil, errUnsupportedFastJPEG
	}
	var ratio image.YCbCrSubsampleRatio
	switch {
	case h0 == 1 && v0 == 1:
		ratio = image.YCbCrSubsampleRatio444
	case h0 == 2 && v0 == 1:
		ratio = image.YCbCrSubsampleRatio422
	case h0 == 2 && v0 == 2:
		ratio = image.YCbCrSubsampleRatio420
	case h0 == 1 && v0 == 2:
		ratio = image.YCbCrSubsampleRatio440
	default:
		return nil, errUnsupportedFastJPEG
	}

	out := image.NewYCbCr(image.Rect(0, 0, bw, bh), ratio)
	mcuCols := (bw + h0 - 1) / h0
	mcuRows := (bh + v0 - 1) / v0

	yDC, yAC, yQ := &dcTables[comps[0].dcHT], &acTables[comps[0].acHT], qtDC[comps[0].qtID]
	cbDC, cbAC, cbQ := &dcTables[comps[1].dcHT], &acTables[comps[1].acHT], qtDC[comps[1].qtID]
	crDC, crAC, crQ := &dcTables[comps[2].dcHT], &acTables[comps[2].acHT], qtDC[comps[2].qtID]

	var dcPred [3]int
	mcuCount := 0
	rstIdx := byte(0)

	for my := 0; my < mcuRows; my++ {
		for mx := 0; mx < mcuCols; mx++ {
			if restartInt > 0 && mcuCount > 0 && mcuCount%restartInt == 0 {
				if err := br.consumeRestartMarker(0xD0 + (rstIdx & 7)); err != nil {
					return nil, err
				}
				rstIdx++
				dcPred = [3]int{}
			}
			for vy := 0; vy < v0; vy++ {
				py := my*v0 + vy
				for vx := 0; vx < h0; vx++ {
					px := mx*h0 + vx
					dc, err := decodeDCBlock(&br, yDC, yAC, &dcPred[0])
					if err != nil {
						return nil, err
					}
					if px < bw && py < bh {
						out.Y[py*out.YStride+px] = clampDCByte(dc, yQ)
					}
				}
			}
			cbVal, err := decodeDCBlock(&br, cbDC, cbAC, &dcPred[1])
			if err != nil {
				return nil, err
			}
			crVal, err := decodeDCBlock(&br, crDC, crAC, &dcPred[2])
			if err != nil {
				return nil, err
			}
			if my*out.CStride+mx < len(out.Cb) {
				cOff := my*out.CStride + mx
				out.Cb[cOff] = clampDCByte(cbVal, cbQ)
				out.Cr[cOff] = clampDCByte(crVal, crQ)
			}
			mcuCount++
		}
	}

	return out, nil
}

func decodeDCBlock(br *bitReader, dcHT, acHT *huffTable, dcPred *int) (int, error) {
	cat, err := br.decodeHuff(dcHT)
	if err != nil {
		return 0, err
	}
	if cat > 11 {
		return 0, errUnsupportedFastJPEG
	}
	diff, err := br.receiveExtend(int(cat))
	if err != nil {
		return 0, err
	}
	dc := *dcPred + diff
	*dcPred = dc

	// Skip the 63 AC coefficients without performing IDCT.
	for k := 1; k < 64; {
		sym, err := br.decodeHuff(acHT)
		if err != nil {
			return 0, err
		}
		if sym == 0x00 { // EOB
			break
		}
		run := int(sym >> 4)
		size := int(sym & 0x0F)
		if size == 0 {
			if run != 15 { // Only ZRL (0xF0) has size==0 aside from EOB
				return 0, errUnsupportedFastJPEG
			}
			k += 16
			continue
		}
		k += run + 1
		if k > 64 {
			return 0, errUnsupportedFastJPEG
		}
		if err := br.skipBits(size); err != nil {
			return 0, err
		}
	}
	return dc, nil
}

func clampDCByte(dc, q0 int) uint8 {
	v := ((dc*q0 + 4) >> 3) + 128
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func decodeJPEGScale8File(filePath string, minResultLongEdge int) (image.Image, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// Quick header check before reading the JPEG into memory: only read files
	// that start with JPEG SOI and are large enough to benefit from 1/8 decode.
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, errUnsupportedFastJPEG
	}
	if hdr[0] != 0xFF || hdr[1] != 0xD8 {
		return nil, errUnsupportedFastJPEG
	}
	info, err := f.Stat()
	if err != nil || info.Size() < 128*1024 || info.Size() > 32*1024*1024 {
		return nil, errUnsupportedFastJPEG
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, errUnsupportedFastJPEG
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, errUnsupportedFastJPEG
	}
	if max((cfg.Width+7)/8, (cfg.Height+7)/8) < minResultLongEdge {
		return nil, errUnsupportedFastJPEG
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, errUnsupportedFastJPEG
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return decodeJPEGScale8Bytes(raw, minResultLongEdge)
}
