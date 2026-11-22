package handoff

import (
	"encoding/json"
	"testing"
	"time"
)

func TestStoreLifecycleAndExpiry(t *testing.T) {
	s := NewStore(5 * time.Minute)
	now := time.Unix(1700000000, 0).UTC()
	s.nowFn = func() time.Time { return now }

	e := s.Create(json.RawMessage(`{"v":1,"assetId":"abc"}`))
	if len(e.ID) != 8 {
		t.Fatalf("expected 8-char base62 ID, got %q", e.ID)
	}
	got, ok := s.Get(e.ID)
	if !ok || string(got.Payload) != `{"v":1,"assetId":"abc"}` {
		t.Fatalf("expected payload back, got ok=%v payload=%s", ok, string(got.Payload))
	}

	now = now.Add(6 * time.Minute)
	if _, ok := s.Get(e.ID); ok {
		t.Fatalf("expected entry to expire after TTL")
	}
}
