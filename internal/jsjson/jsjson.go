// Package jsjson provides the ECMAScript-compatible primitives the canonical
// .edits serialization is defined in terms of: UTF-16 code-unit string order,
// object property enumeration order, JSON.stringify string quoting, and
// Number-to-string formatting.
//
// The canonical form of a sidecar is "what JSON.stringify emits for a
// recursively key-sorted object". Editors that run in a browser produce that
// form natively, so a second implementation is byte-compatible only if it
// reproduces these four rules exactly rather than Go's native ones (UTF-8 byte
// order, HTML/U+2028 escaping, verbatim number literals).
package jsjson

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"unicode/utf8"
)

// CompareUTF16 orders a and b by UTF-16 code units, the order of the
// ECMAScript `<` operator and of Array.prototype.sort without a comparator.
// It differs from byte order only when a supplementary-plane character
// (encoded as a surrogate pair, 0xD800-0xDFFF) meets a character in
// U+E000-U+FFFF. Returns -1, 0, or 1.
func CompareUTF16(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			a1, a2 := utf16Units(ra)
			b1, b2 := utf16Units(rb)
			if a1 != b1 {
				if a1 < b1 {
					return -1
				}
				return 1
			}
			if a2 < b2 {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case len(a) == len(b):
		return 0
	case len(a) == 0:
		return -1
	default:
		return 1
	}
}

// utf16Units returns the UTF-16 encoding of r as (first, second); second is 0
// for a Basic Multilingual Plane character.
func utf16Units(r rune) (uint16, uint16) {
	if r < 0x10000 {
		return uint16(r), 0
	}
	r -= 0x10000
	return 0xD800 + uint16(r>>10), 0xDC00 + uint16(r&0x3FF)
}

// ArrayIndex reports whether key is an ECMAScript array index: the canonical
// decimal form of an integer in [0, 2^32-2]. Such keys are enumerated before
// all other keys, in ascending numeric order, regardless of insertion order.
func ArrayIndex(key string) (uint32, bool) {
	if key == "" || len(key) > 10 || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	if n > math.MaxUint32-1 {
		return 0, false
	}
	return uint32(n), true
}

// SortKeys orders object keys the way a key-sorted ECMAScript object
// enumerates them: array-index keys first in ascending numeric order, then
// every other key in UTF-16 code-unit order.
func SortKeys(keys []string) {
	sort.Slice(keys, func(i, j int) bool {
		ai, aIsIndex := ArrayIndex(keys[i])
		bi, bIsIndex := ArrayIndex(keys[j])
		switch {
		case aIsIndex && bIsIndex:
			return ai < bi
		case aIsIndex != bIsIndex:
			return aIsIndex
		default:
			return CompareUTF16(keys[i], keys[j]) < 0
		}
	})
}

const hexDigits = "0123456789abcdef"

// AppendString appends s as a JSON string literal exactly as JSON.stringify
// quotes it: only `"`, `\`, and C0 controls are escaped (with the short forms
// \b \t \n \f \r); every other character, including `<`, `>`, `&`, U+007F,
// U+2028 and U+2029, is emitted as raw UTF-8. Bytes that are not valid UTF-8
// are emitted as U+FFFD.
func AppendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				dst = append(dst, '\\', c)
			case c >= 0x20:
				dst = append(dst, c)
			case c == '\b':
				dst = append(dst, '\\', 'b')
			case c == '\t':
				dst = append(dst, '\\', 't')
			case c == '\n':
				dst = append(dst, '\\', 'n')
			case c == '\f':
				dst = append(dst, '\\', 'f')
			case c == '\r':
				dst = append(dst, '\\', 'r')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = utf8.AppendRune(dst, utf8.RuneError)
		} else {
			dst = append(dst, s[i:i+size]...)
		}
		i += size
	}
	return append(dst, '"')
}

// AppendFloat appends f as JSON.stringify formats a Number: the shortest
// decimal that round-trips, positional for 1e-6 <= |f| < 1e21 and exponential
// (without exponent zero-padding) otherwise, "0" for both zeros, and "null"
// for NaN and the infinities.
func AppendFloat(dst []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(dst, "null"...)
	}
	if f == 0 {
		return append(dst, '0')
	}
	abs := math.Abs(f)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.AppendFloat(dst, f, 'f', -1, 64)
	}
	start := len(dst)
	dst = strconv.AppendFloat(dst, f, 'e', -1, 64)
	// strconv pads the exponent to two digits ("1e-07"); ECMAScript does not.
	if n := len(dst); n-start >= 4 && dst[n-2] == '0' && (dst[n-3] == '-' || dst[n-3] == '+') && dst[n-4] == 'e' {
		dst[n-2] = dst[n-1]
		dst = dst[:n-1]
	}
	return dst
}

// AppendNumberLiteral appends a JSON number literal the way a JSON.parse ->
// JSON.stringify round trip rewrites it: parsed as an IEEE-754 double and
// formatted by AppendFloat. "1.0" becomes "1", "1e21" becomes "1e+21", "-0"
// becomes "0", and an out-of-range literal such as "1e999" becomes "null".
func AppendNumberLiteral(dst []byte, literal string) ([]byte, error) {
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return dst, err
	}
	return AppendFloat(dst, f), nil
}
