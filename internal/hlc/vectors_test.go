package hlc

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type hlcVectors struct {
	MaxDriftMillis int64 `json:"maxDriftMillis"`
	Parse          []struct {
		S       string `json:"s"`
		OK      bool   `json:"ok"`
		Millis  uint64 `json:"millis"`
		Counter uint32 `json:"counter"`
		Device  string `json:"device"`
	} `json:"parse"`
	Compare []struct {
		A, B string
		Want int
	} `json:"compare"`
	Clock []struct {
		Name     string   `json:"name"`
		Device   string   `json:"device"`
		Now      int64    `json:"now"`
		Observe  []string `json:"observe"`
		Observed []bool   `json:"observed"`
		Ticks    []string `json:"ticks"`
	} `json:"clock"`
	Drift []struct {
		Now    int64  `json:"now"`
		HLC    string `json:"hlc"`
		Beyond bool   `json:"beyond"`
	} `json:"drift"`
}

// TestSharedHLCVectors runs testdata/hlc_vectors.json, the cases the browser
// editor's HLC implementation runs too: which strings are HLCs, how they
// order, what a clock issues after what it observed, and where the drift
// bound lies. A case must never be edited to make one side pass.
func TestSharedHLCVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hlc_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v hlcVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.MaxDriftMillis != MaxDrift.Milliseconds() {
		t.Fatalf("MaxDrift = %d ms, vectors say %d", MaxDrift.Milliseconds(), v.MaxDriftMillis)
	}
	if len(v.Parse) == 0 || len(v.Compare) == 0 || len(v.Clock) == 0 || len(v.Drift) == 0 {
		t.Fatal("vector file is missing a section")
	}

	for _, c := range v.Parse {
		p, err := Parse(c.S)
		if (err == nil) != c.OK {
			t.Errorf("Parse(%q): err=%v, want ok=%v", c.S, err, c.OK)
			continue
		}
		if c.OK && (p.Millis != c.Millis || p.Counter != c.Counter || p.Device != c.Device) {
			t.Errorf("Parse(%q) = %+v, want (%d, %d, %q)", c.S, p, c.Millis, c.Counter, c.Device)
		}
	}
	for _, c := range v.Compare {
		if got, err := Compare(c.A, c.B); err != nil || got != c.Want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d", c.A, c.B, got, err, c.Want)
		}
		if got, err := Compare(c.B, c.A); err != nil || got != -c.Want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d", c.B, c.A, got, err, -c.Want)
		}
	}
	for _, c := range v.Clock {
		now := time.UnixMilli(c.Now)
		clk := NewClock(c.Device, func() time.Time { return now })
		for i, remote := range c.Observe {
			if observed := clk.Observe(remote) == nil; observed != c.Observed[i] {
				t.Errorf("%s: Observe(%q) observed=%v, want %v", c.Name, remote, observed, c.Observed[i])
			}
		}
		for i, want := range c.Ticks {
			if got := clk.Tick(); got != want {
				t.Errorf("%s: tick %d = %q, want %q", c.Name, i, got, want)
			}
		}
	}
	for _, c := range v.Drift {
		p, err := Parse(c.HLC)
		if err != nil {
			t.Errorf("drift case %q does not parse: %v", c.HLC, err)
			continue
		}
		if got := BeyondDrift(p, time.UnixMilli(c.Now)); got != c.Beyond {
			t.Errorf("BeyondDrift(%q, now=%d) = %v, want %v", c.HLC, c.Now, got, c.Beyond)
		}
	}
}

// One far-future HLC must not move the clock: everything the clock issued
// afterwards would carry that timestamp and win last-writer-wins against every
// other device until real time caught up.
func TestObserveDoesNotFollowAnHLCBeyondTheDriftBound(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	clk := NewClock("srv", func() time.Time { return now })
	before := clk.Tick()
	if err := clk.Observe("1fffffffffffff-ffffffff-z"); !errors.Is(err, ErrBeyondDrift) {
		t.Fatalf("Observe of the largest HLC: err=%v, want ErrBeyondDrift", err)
	}
	after := clk.Tick()
	if p, _ := Parse(after); p.Millis != uint64(now.UnixMilli()) {
		t.Fatalf("clock moved to %q after an HLC beyond the bound (was %q)", after, before)
	}
}
