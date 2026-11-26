// Package hlc implements Hybrid Logical Clock parsing, canonical formatting,
// numeric total-order comparison, and a monotonic local clock compatible with
// the hdr.toys .edits sidecar format.
package hlc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thebigsasha/cloudedits/internal/jsjson"
)

// Parsed represents the decoded components of a canonical HLC string:
// "<unixMillis-hex>-<counter-hex>-<deviceId>".
type Parsed struct {
	Millis  uint64
	Counter uint32
	Device  string
}

const (
	// MaxMillis is the largest timestamp an HLC may carry: 2^53-1, the largest
	// integer a browser editor represents exactly. A larger one would compare
	// differently in the two implementations.
	MaxMillis uint64 = 1<<53 - 1
	// MaxCounter is the largest logical counter an HLC may carry.
	MaxCounter uint32 = 1<<32 - 1
	// MaxDrift is how far ahead of the receiver's wall clock an HLC may be.
	// Last-writer-wins trusts the timestamp, so one edit stamped far in the
	// future would win against every later edit, and a clock that folded it
	// in would stamp everything it issues there too. An HLC further ahead is
	// not observed by any clock and not accepted as an op.
	MaxDrift = 24 * time.Hour
)

// ErrBeyondDrift reports an HLC further ahead of the wall clock than MaxDrift.
var ErrBeyondDrift = errors.New("HLC is ahead of the clock by more than the allowed drift")

// BeyondDrift reports whether p is further ahead of now than MaxDrift.
func BeyondDrift(p Parsed, now time.Time) bool {
	return p.Millis > uint64(max(now.UnixMilli(), 0))+uint64(MaxDrift.Milliseconds())
}

// Format formats an HLC into its canonical string representation:
// 12-digit lowercase hex milliseconds, 4-digit lowercase hex counter, deviceId.
func Format(millis uint64, counter uint32, device string) string {
	return fmt.Sprintf("%012x-%04x-%s", millis, counter, device)
}

func isStrictHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

// Parse strictly parses an HLC string, splitting only on the first two '-'
// separators so device IDs may safely contain hyphens.
func Parse(s string) (Parsed, error) {
	first := strings.IndexByte(s, '-')
	if first <= 0 {
		return Parsed{}, fmt.Errorf("invalid HLC %q: missing first separator", s)
	}
	rest := s[first+1:]
	second := strings.IndexByte(rest, '-')
	if second <= 0 {
		return Parsed{}, fmt.Errorf("invalid HLC %q: missing second separator", s)
	}

	msHex := s[:first]
	cntHex := rest[:second]
	device := rest[second+1:]
	if device == "" {
		return Parsed{}, fmt.Errorf("invalid HLC %q: empty device id", s)
	}
	if !isStrictHex(msHex) {
		return Parsed{}, fmt.Errorf("invalid HLC %q: non-hex millis %q", s, msHex)
	}
	if !isStrictHex(cntHex) {
		return Parsed{}, fmt.Errorf("invalid HLC %q: non-hex counter %q", s, cntHex)
	}

	ms, err := strconv.ParseUint(msHex, 16, 64)
	if err != nil {
		return Parsed{}, fmt.Errorf("invalid HLC %q: millis overflow: %w", s, err)
	}
	if ms > MaxMillis {
		return Parsed{}, fmt.Errorf("invalid HLC %q: millis exceed 2^53-1", s)
	}
	cnt, err := strconv.ParseUint(cntHex, 16, 32)
	if err != nil {
		return Parsed{}, fmt.Errorf("invalid HLC %q: counter overflow: %w", s, err)
	}
	return Parsed{Millis: ms, Counter: uint32(cnt), Device: device}, nil
}

// Compare compares two HLC strings numerically on (Millis, Counter) and then
// on Device in UTF-16 code-unit order (the order browser editors compare
// device ids in). Returns -1, 0, or 1.
func Compare(a, b string) (int, error) {
	if a == b {
		if _, err := Parse(a); err != nil {
			return 0, err
		}
		return 0, nil
	}
	pa, err := Parse(a)
	if err != nil {
		return 0, err
	}
	pb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	if pa.Millis != pb.Millis {
		if pa.Millis < pb.Millis {
			return -1, nil
		}
		return 1, nil
	}
	if pa.Counter != pb.Counter {
		if pa.Counter < pb.Counter {
			return -1, nil
		}
		return 1, nil
	}
	return jsjson.CompareUTF16(pa.Device, pb.Device), nil
}

// Clock is a thread-safe monotonic Hybrid Logical Clock.
type Clock struct {
	mu          sync.Mutex
	device      string
	nowFn       func() time.Time
	lastMillis  uint64
	lastCounter uint32
}

// NewClock creates a new monotonic HLC clock for the given device ID.
func NewClock(device string, nowFn func() time.Time) *Clock {
	if device == "" {
		device = "cloudedits"
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Clock{
		device: device,
		nowFn:  nowFn,
	}
}

// Tick produces a strictly greater canonical HLC string.
func (c *Clock) Tick() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	nowMs := uint64(max(c.nowFn().UnixMilli(), 0))
	switch {
	case nowMs > c.lastMillis:
		c.lastMillis = nowMs
		c.lastCounter = 0
	case c.lastCounter == MaxCounter:
		// The counter is exhausted for this logical millisecond (an observed
		// remote HLC can carry any counter): carry into the next millisecond
		// rather than wrapping to zero and issuing an earlier timestamp. At
		// the largest representable HLC the clock saturates.
		if c.lastMillis < MaxMillis {
			c.lastMillis++
			c.lastCounter = 0
		}
	default:
		c.lastCounter++
	}
	return Format(c.lastMillis, c.lastCounter, c.device)
}

// Observe folds a remote HLC into the local clock so subsequent Tick calls are
// strictly greater than both local wall time and the observed remote HLC. An
// HLC beyond MaxDrift is not folded in and reports ErrBeyondDrift.
func (c *Clock) Observe(remote string) error {
	p, err := Parse(remote)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if BeyondDrift(p, c.nowFn()) {
		return fmt.Errorf("%w: %q", ErrBeyondDrift, remote)
	}

	switch {
	case p.Millis > c.lastMillis:
		c.lastMillis = p.Millis
		c.lastCounter = p.Counter
	case p.Millis == c.lastMillis && p.Counter > c.lastCounter:
		c.lastCounter = p.Counter
	}
	return nil
}

// ErrInvalidHLC is returned when an HLC fails validation.
var ErrInvalidHLC = errors.New("invalid HLC")
