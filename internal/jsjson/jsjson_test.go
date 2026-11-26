package jsjson

import (
	"math"
	"slices"
	"testing"
)

func TestCompareUTF16OrdersBySurrogateCodeUnits(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "a", 0},
		{"", "a", -1},
		{"a", "", 1},
		{"a", "b", -1},
		{"B", "a", -1},
		{"ab", "a", 1},
		{"\u00E9", "z", 1},
		// U+1F600 is the surrogate pair D83D DE00, which sorts BEFORE U+FF5E in
		// UTF-16 code units even though its UTF-8 bytes sort after.
		{"\U0001F600", "\uFF5E", -1},
		{"\uFF5E", "\U0001F600", 1},
		{"\U0001F600", "\uE000", -1},
		{"\U0001F600", "\uD7FF", 1},
		// Same high surrogate, different low surrogate.
		{"\U0001F600", "\U0001F601", -1},
		{"x\U00010000", "x\uFFFF", -1},
	}
	for _, c := range cases {
		if got := CompareUTF16(c.a, c.b); got != c.want {
			t.Errorf("CompareUTF16(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSortKeysHoistsArrayIndexKeys(t *testing.T) {
	keys := []string{"b", "4294967295", "4294967294", "10", "9", "a", "01", "1.0", "-1", "1e3", "0", "00", "\uFF5E", "\U0001F600", ""}
	SortKeys(keys)
	want := []string{"0", "9", "10", "4294967294", "", "-1", "00", "01", "1.0", "1e3", "4294967295", "a", "b", "\U0001F600", "\uFF5E"}
	if !slices.Equal(keys, want) {
		t.Fatalf("SortKeys order:\n got %q\nwant %q", keys, want)
	}
}

func TestAppendStringMatchesJSONStringify(t *testing.T) {
	cases := map[string]string{
		"plain":                  `"plain"`,
		`q"b\s/`:                 `"q\"b\\s/"`,
		"<script>&amp;</script>": `"<script>&amp;</script>"`,
		"a\u2028b\u2029c":        "\"a\u2028b\u2029c\"",
		"\x00\x01\b\f\n\r\t\x1f": `"\u0000\u0001\b\f\n\r\t\u001f"`,
		"\x7f\u0080\uFEFF\uFFFD": "\"\x7f\u0080\uFEFF\uFFFD\"",
		"\U0001F600":             "\"\U0001F600\"",
		"bad\xffutf8":            "\"bad\uFFFDutf8\"",
	}
	for in, want := range cases {
		if got := string(AppendString(nil, in)); got != want {
			t.Errorf("AppendString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAppendFloatMatchesNumberToString(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"}, {math.Copysign(0, -1), "0"}, {1, "1"}, {-1.5, "-1.5"}, {100, "100"},
		{0.30000000000000004, "0.30000000000000004"}, {1e21, "1e+21"}, {123456789012345680000, "123456789012345680000"},
		{1e-7, "1e-7"}, {1.5e-7, "1.5e-7"}, {0.000001, "0.000001"}, {5e-324, "5e-324"},
		{1e300, "1e+300"}, {1.7976931348623157e308, "1.7976931348623157e+308"}, {1.23e-18, "1.23e-18"},
		{9007199254740992, "9007199254740992"}, {math.Inf(1), "null"}, {math.Inf(-1), "null"}, {math.NaN(), "null"},
	}
	for _, c := range cases {
		if got := string(AppendFloat(nil, c.in)); got != c.want {
			t.Errorf("AppendFloat(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestAppendNumberLiteralNormalizesLikeParseStringify(t *testing.T) {
	cases := map[string]string{
		"5": "5", "5.0": "5", "1.50": "1.5", "-0": "0", "-0.0": "0", "1e21": "1e+21", "1E21": "1e+21",
		"1e20": "100000000000000000000", "1e+2": "100", "-1.25E-3": "-0.00125", "100.000": "100",
		"9007199254740993": "9007199254740992", "123456789012345678901234567890": "1.2345678901234568e+29",
		"1e-400": "0", "1e999": "null", "-1e999": "null",
	}
	for in, want := range cases {
		got, err := AppendNumberLiteral(nil, in)
		if err != nil {
			t.Errorf("AppendNumberLiteral(%q) error: %v", in, err)
			continue
		}
		if string(got) != want {
			t.Errorf("AppendNumberLiteral(%q) = %s, want %s", in, got, want)
		}
	}
	if _, err := AppendNumberLiteral(nil, "not-a-number"); err == nil {
		t.Error("AppendNumberLiteral accepted a non-numeric literal")
	}
}
