package hlc

import (
	"strings"
	"testing"
)

// FuzzParseCompare checks that Parse never panics, that every accepted HLC
// survives a Format round trip with identical ordering, and that Compare is a
// consistent total order (reflexive, antisymmetric) over accepted inputs.
func FuzzParseCompare(f *testing.F) {
	for _, s := range []string{
		"019a2f0012ab-0001-devA",
		"000000000001-0000-v1",
		"ff-100-device-with-hyphens",
		"FFFFFFFFFFFFFFFF-FFFFFFFF-z",
		"10000000000000000-0-overflow",
		"0-100000000-overflow",
		"--", "-0-a", "0--a", "0-0-", "", "zz-0-a", "0x1-0-a", "+1-0-a", " 1-0-a",
	} {
		f.Add(s, "019a2f0012ab-0002-devB")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		pa, errA := Parse(a)
		_, errB := Parse(b)
		if errA == nil {
			if pa.Device == "" {
				t.Fatalf("Parse(%q) accepted an empty device id", a)
			}
			if !strings.HasSuffix(a, "-"+pa.Device) {
				t.Fatalf("Parse(%q) device %q is not the input suffix", a, pa.Device)
			}
			canon := Format(pa.Millis, pa.Counter, pa.Device)
			pc, err := Parse(canon)
			if err != nil || pc != pa {
				t.Fatalf("Format round trip of %q -> %q failed: %+v %v", a, canon, pc, err)
			}
			if c, err := Compare(a, canon); err != nil || c != 0 {
				t.Fatalf("Compare(%q, canonical %q) = %d, %v; want 0", a, canon, c, err)
			}
			if c, err := Compare(a, a); err != nil || c != 0 {
				t.Fatalf("Compare(%q, itself) = %d, %v", a, c, err)
			}
		}
		ab, errAB := Compare(a, b)
		ba, errBA := Compare(b, a)
		if (errA == nil && errB == nil) != (errAB == nil) || (errAB == nil) != (errBA == nil) {
			t.Fatalf("Compare error mismatch for %q vs %q: parse(%v,%v) compare(%v,%v)", a, b, errA, errB, errAB, errBA)
		}
		if errAB == nil && ab != -ba {
			t.Fatalf("Compare not antisymmetric for %q vs %q: %d and %d", a, b, ab, ba)
		}

		c := NewClock("fuzz", nil)
		before := c.Tick()
		if err := c.Observe(a); (err == nil) != (errA == nil) {
			t.Fatalf("Observe(%q) error %v disagrees with Parse error %v", a, err, errA)
		}
		after := c.Tick()
		if cmp, err := Compare(after, before); err != nil || cmp <= 0 {
			t.Fatalf("clock went backwards after Observe(%q): %q then %q", a, before, after)
		}
		if errA == nil {
			if cmp, err := Compare(after, a); err != nil || cmp <= 0 {
				// Only the single largest representable HLC cannot be exceeded.
				if pa.Counter != ^uint32(0) || pa.Millis != ^uint64(0) {
					t.Fatalf("Tick %q is not greater than observed %q", after, a)
				}
			}
		}
	})
}
