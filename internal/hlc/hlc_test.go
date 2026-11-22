package hlc

import (
	"testing"
	"time"
)

func TestParseAndNumericCompare(t *testing.T) {
	// Variable-width hex: 0x100 > 0xff numerically, even though "100" < "ff" lexically.
	cmp, err := Compare("ff-0000-devA", "100-0000-devA")
	if err != nil {
		t.Fatalf("Compare failed: %v", err)
	}
	if cmp >= 0 {
		t.Fatalf("expected ff < 100 numerically, got %d", cmp)
	}

	// Hyphens inside device ID must be preserved intact.
	p, err := Parse("019a2f0012ab-000a-mac-studio-home-1")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if p.Device != "mac-studio-home-1" {
		t.Fatalf("unexpected device: %q", p.Device)
	}
	if p.Counter != 0xa {
		t.Fatalf("unexpected counter: %d", p.Counter)
	}

	// Counter tie-break and device tie-break.
	cmp, err = Compare("019a2f0012ab-0001-devB", "019a2f0012ab-0002-devA")
	if err != nil || cmp >= 0 {
		t.Fatalf("expected counter 1 < counter 2, got cmp=%d err=%v", cmp, err)
	}
	cmp, err = Compare("019a2f0012ab-0001-devA", "019a2f0012ab-0001-devB")
	if err != nil || cmp >= 0 {
		t.Fatalf("expected devA < devB on equal millis/counter, got cmp=%d err=%v", cmp, err)
	}

	// Malformed HLCs must error cleanly.
	bad := []string{"", "not-an-hlc", "zzzz-0000-dev", "019a-gg-dev", "019a-0001-", "-0001-dev"}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("expected error for malformed HLC %q", s)
		}
	}
}

func TestClockMonotonicAndObserve(t *testing.T) {
	now := time.UnixMilli(1700000000000)
	clk := NewClock("srv-1", func() time.Time { return now })

	h1 := clk.Tick()
	h2 := clk.Tick()
	cmp, err := Compare(h1, h2)
	if err != nil || cmp >= 0 {
		t.Fatalf("expected h1 < h2, got %q vs %q (cmp=%d err=%v)", h1, h2, cmp, err)
	}

	future := Format(1800000000000, 5, "remote-dev")
	if err := clk.Observe(future); err != nil {
		t.Fatalf("Observe failed: %v", err)
	}
	h3 := clk.Tick()
	cmp, err = Compare(future, h3)
	if err != nil || cmp >= 0 {
		t.Fatalf("expected observed future < h3, got %q vs %q", future, h3)
	}
}
