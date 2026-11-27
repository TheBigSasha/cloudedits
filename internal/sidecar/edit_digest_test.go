package sidecar

import (
	"encoding/json"
	"math"
	"testing"
)

// The index compares the digest of the state an op batch left in memory with
// the digest of the same state read back from disk. If the two could differ
// (a number that serializes one way from a Go value and another way after a
// round trip), every write would look like an edit made by another program.
func TestEditDigestIsTheSameBeforeAndAfterARoundTrip(t *testing.T) {
	name := "Snapshot   one"
	sc := Empty("A.JPG", nil, "")
	ops := []TruthOp{
		{HLC: "019a2f001000-0001-d", Device: "d", Field: "rating", Value: 3},
		{HLC: "019a2f001000-0002-d", Device: "d", Field: "exposure", Value: 0.1},
		{HLC: "019a2f001000-0003-d", Device: "d", Field: "big", Value: 1e21},
		{HLC: "019a2f001000-0004-d", Device: "d", Field: "negzero", Value: math.Copysign(0, -1)},
		{HLC: "019a2f001000-0005-d", Device: "d", Field: "wire", Value: json.Number("12345678901234567890")},
		{HLC: "019a2f001000-0006-d", Device: "d", Field: "keywords", Value: []any{"b", "a", "é"}},
		{HLC: "019a2f001000-0007-d", Device: "d", Field: "recipe", Value: map[string]any{
			"10": 1, "2": 2, "b": []any{1.5, json.Number("2.50"), nil, true}, "a": map[string]any{"x": float32(0.25)},
		}},
		{HLC: "019a2f001000-0008-d", Device: "d", Field: "variant", Value: map[string]any{
			"id": "v1", "name": name, "recipe": map[string]any{"k": 1}, "snapshotKind": "manual",
		}},
		{HLC: "019a2f001000-0009-d", Device: "d", Field: "variant", Value: map[string]any{"id": "v0", "tombstone": true}},
	}
	for _, op := range ops {
		if _, err := ApplyOp(sc, op); err != nil {
			t.Fatalf("apply %s: %v", op.Field, err)
		}
	}
	inMemory := EditDigest(sc)

	payload, err := Serialize(sc)
	if err != nil {
		t.Fatal(err)
	}
	reread, err := Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk := EditDigest(reread); onDisk != inMemory || inMemory == "" {
		t.Fatalf("digest in memory %q, after a round trip %q", inMemory, onDisk)
	}

	// It follows the edits and nothing else.
	reread.Asset["bytes"] = json.Number("999")
	reread.Provenance = map[string]any{"tool": "x"}
	if EditDigest(reread) != inMemory {
		t.Error("digest changed with the asset block or provenance")
	}
	if _, err := ApplyOp(reread, TruthOp{HLC: "019a2f002000-0001-d", Device: "d", Field: "rating", Value: 3}); err != nil {
		t.Fatal(err)
	}
	if EditDigest(reread) == inMemory {
		t.Error("digest did not change when a cell's HLC changed")
	}
	if EditDigest(Empty("A.JPG", nil, "")) != "" || EditDigest(nil) != "" {
		t.Error("a sidecar without edits must have the empty digest")
	}
}
