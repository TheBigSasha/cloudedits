package hlc

import (
	"testing"
	"time"
)

// A remote HLC may carry any counter. After observing one at the 32-bit
// ceiling the next Tick must still be greater: wrapping the counter to zero
// would issue a timestamp that loses to the value just observed.
func TestTickCarriesCounterOverflowIntoMillis(t *testing.T) {
	fixed := time.UnixMilli(1_700_000_000_000)
	c := NewClock("srv", func() time.Time { return fixed })
	observed := Format(1_800_000_000_000, 0xFFFFFFFF, "peer")
	if err := c.Observe(observed); err != nil {
		t.Fatal(err)
	}
	prev := observed
	for i := 0; i < 3; i++ {
		next := c.Tick()
		if cmp, err := Compare(next, prev); err != nil || cmp <= 0 {
			t.Fatalf("Tick %d = %q is not greater than %q (cmp=%d, err=%v)", i, next, prev, cmp, err)
		}
		prev = next
	}
	if want := Format(1_800_000_000_001, 2, "srv"); prev != want {
		t.Fatalf("after carry: %q, want %q", prev, want)
	}
}
