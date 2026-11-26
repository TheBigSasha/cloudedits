package sidecar

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addGoldenEditsSeeds(f *testing.F) {
	f.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", "testdata", "golden_edits", "*.edits"))
	if err != nil || len(matches) == 0 {
		f.Fatalf("golden .edits seed corpus missing: %v", err)
	}
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
}

// FuzzParseSerialize checks the .edits parser and canonical serialiser:
// arbitrary bytes never panic, every accepted document re-serialises to valid
// JSON that parses again, and canonicalisation is a fixed point (serialising
// twice yields identical bytes), ends with one newline and leads with "hdrt".
func FuzzParseSerialize(f *testing.F) {
	addGoldenEditsSeeds(f)
	for _, s := range []string{
		`{"hdrt":1,"asset":{}}`,
		`{"hdrt":1,"asset":{},"fields":{"rating":{"hlc":"1-0-a","v":1e999}}}`,
		`{"hdrt":1,"asset":{},"variants":[{"id":"\u2028","hlc":"1-0-a","name":5,"tombstone":"x","recipe":null}]}`,
		`{"hdrt":1,"asset":{},"document":null,"recipe":null,"variants":null,"provenance":null}`,
		`{"hdrt":9007199254740993,"asset":{"file":"\ud800"},"__proto__":{"a":[[[[[]]]]]}}`,
		`{"hdrt":1,"hdrt":2,"asset":{},"asset":{"k":"\u0000"}} trailing`,
		`[]`, `null`, `{"hdrt":0,"asset":{}}`, `{"hdrt":1.5,"asset":{}}`, `{"hdrt":"1","asset":{}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		sc, err := Parse(data)
		if err != nil {
			return
		}
		first, err := Serialize(sc)
		if err != nil {
			t.Fatalf("Serialize of accepted document failed: %v", err)
		}
		if !json.Valid(first) {
			t.Fatalf("Serialize emitted invalid JSON:\n%s", first)
		}
		if !bytes.HasPrefix(first, []byte("{\n  \"hdrt\": ")) || !bytes.HasSuffix(first, []byte("\n}\n")) {
			t.Fatalf("Serialize output is not canonical framed:\n%s", first)
		}
		sc2, err := Parse(first)
		if err != nil {
			t.Fatalf("canonical output does not re-parse: %v\n%s", err, first)
		}
		second, err := Serialize(sc2)
		if err != nil {
			t.Fatalf("second Serialize failed: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("canonicalisation is not a fixed point:\n--- first\n%s\n--- second\n%s", first, second)
		}

		// Self-merge and verdict extraction must tolerate any accepted document.
		merged, err := Merge(sc, sc2, MergeOptions{RescueConflictingRecipes: true})
		if err == nil && merged != nil {
			if _, err := Serialize(merged); err != nil {
				t.Fatalf("Serialize of self-merge failed: %v", err)
			}
		}
		_, _, _, _, _ = ExtractVerdicts(sc)
		_ = LatestHLC(sc)
	})
}

// FuzzSafeJoinRel checks that no relative path (traversal, absolute, backslash,
// NUL or dot-segment tricks) resolves outside the base directory.
func FuzzSafeJoinRel(f *testing.F) {
	for _, s := range []string{
		"a/b.jpg", "../x", "..", "a/../../x", "/etc/passwd", "a\\..\\..\\x", "..\\..\\x",
		"a/./b", "a//b", "./", "", ".", "...", "a/..", "a\x00b", "C:\\Windows", "//server/share",
		"%2e%2e/x", "a/…/b", ".hdrtoys/../../x", "renders/../../../x",
	} {
		f.Add(s)
	}
	base := filepath.Join(string(filepath.Separator), "library", "root")
	f.Fuzz(func(t *testing.T, rel string) {
		for _, suffix := range []string{"", ".edits", ".edit_thumbnail.jpg"} {
			full, err := SafeJoinRel(base, rel+suffix)
			if err != nil {
				continue
			}
			cleaned := filepath.Clean(full)
			if cleaned != full {
				t.Fatalf("SafeJoinRel(%q) returned non-clean path %q", rel+suffix, full)
			}
			if !strings.HasPrefix(cleaned, base+string(filepath.Separator)) {
				t.Fatalf("SafeJoinRel(%q) = %q escapes base %q", rel+suffix, full, base)
			}
			for _, seg := range strings.Split(strings.TrimPrefix(cleaned, base), string(filepath.Separator)) {
				if seg == ".." {
					t.Fatalf("SafeJoinRel(%q) = %q retains a parent segment", rel+suffix, full)
				}
			}
		}
	})
}

// FuzzAuxChannelPath checks that no channel name or relative path yields an
// auxiliary channel path outside the sidecar root or without a recognised
// channel suffix.
func FuzzAuxChannelPath(f *testing.F) {
	for _, c := range []string{"depth", "DEPTH", "depth.png", "../depth", "..", "", "depth/..", "a\\b", "depth\x00", "normal", "d-e_p0"} {
		f.Add("a/b.jpg", c)
		f.Add("../../etc/passwd", c)
	}
	base := filepath.Join(string(filepath.Separator), "library", "root", ".hdrtoys")
	f.Fuzz(func(t *testing.T, rel, channel string) {
		suffix, verr := ValidateAuxChannel(channel)
		full, err := AuxChannelPath(base, rel, channel)
		if verr != nil {
			if err == nil {
				t.Fatalf("AuxChannelPath accepted channel %q that ValidateAuxChannel rejects", channel)
			}
			return
		}
		if channel != strings.ToLower(channel) || strings.ContainsAny(channel, "/\\.\x00") || !strings.HasSuffix(suffix, ".png") {
			t.Fatalf("channel %q validated with suffix %q", channel, suffix)
		}
		if err != nil {
			return
		}
		if filepath.Clean(full) != full || !strings.HasPrefix(full, base+string(filepath.Separator)) || !strings.HasSuffix(full, suffix) {
			t.Fatalf("AuxChannelPath(%q, %q) = %q escapes %q or lacks suffix %q", rel, channel, full, base, suffix)
		}
	})
}
