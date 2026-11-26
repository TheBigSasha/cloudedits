package sidecar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoldenVectorsByteIdentity(t *testing.T) {
	goldenDir := filepath.Join("..", "..", "testdata", "golden_edits")
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("read golden_edits dir: %v", err)
	}
	if len(entries) < 4 {
		t.Fatalf("expected at least 4 golden vectors, got %d", len(entries))
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".edits") {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(goldenDir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			sc, err := Parse(raw)
			if err != nil {
				t.Fatalf("parse %s: %v", e.Name(), err)
			}
			out, err := Serialize(sc)
			if err != nil {
				t.Fatalf("serialize %s: %v", e.Name(), err)
			}
			if string(out) != string(raw) {
				t.Fatalf("golden byte mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s", e.Name(), string(raw), string(out))
			}
		})
	}
}

func TestConflictRescueMatchesGolden(t *testing.T) {
	a := Empty("CONFLICT.ARW", nil, "")
	if _, err := ApplyOp(a, TruthOp{
		HLC:      "019a2f001900-0001-devA",
		Device:   "devA",
		AssetKey: "CONFLICT.ARW",
		Field:    "recipe",
		Value:    map[string]any{"version": 1, "exposure": -0.5},
	}); err != nil {
		t.Fatalf("apply a: %v", err)
	}

	b := Empty("CONFLICT.ARW", nil, "")
	if _, err := ApplyOp(b, TruthOp{
		HLC:      "019a2f002000-0001-devB",
		Device:   "devB",
		AssetKey: "CONFLICT.ARW",
		Field:    "recipe",
		Value:    map[string]any{"version": 1, "exposure": 1.2},
	}); err != nil {
		t.Fatalf("apply b: %v", err)
	}

	ab, err := Merge(a, b, MergeOptions{RescueConflictingRecipes: true})
	if err != nil {
		t.Fatalf("merge(a,b): %v", err)
	}
	ba, err := Merge(b, a, MergeOptions{RescueConflictingRecipes: true})
	if err != nil {
		t.Fatalf("merge(b,a): %v", err)
	}

	abBytes, err := Serialize(ab)
	if err != nil {
		t.Fatalf("serialize ab: %v", err)
	}
	baBytes, err := Serialize(ba)
	if err != nil {
		t.Fatalf("serialize ba: %v", err)
	}
	if string(abBytes) != string(baBytes) {
		t.Fatalf("merge with conflict rescue not commutative:\nAB:\n%s\nBA:\n%s", abBytes, baBytes)
	}
	if len(ab.Variants) != 1 || ab.Variants[0].Name == nil || *ab.Variants[0].Name != "Merged edit" {
		t.Fatalf("expected rescued 'Merged edit' variant, got %+v", ab.Variants)
	}
	if !strings.HasPrefix(ab.Variants[0].ID, "conflict-019a2f001900-0001-devA-") {
		t.Fatalf("unexpected conflict variant id: %s", ab.Variants[0].ID)
	}
	golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden_edits", "04_conflict_rescue.edits"))
	if err != nil {
		t.Fatalf("read 04_conflict_rescue.edits: %v", err)
	}
	if string(abBytes) != string(golden) {
		t.Fatalf("conflict rescue output differs from golden:\n--- want ---\n%s\n--- got ---\n%s", string(golden), string(abBytes))
	}
}

func TestMergeLatticeLawsAndTombstoneDominance(t *testing.T) {
	a := Empty("IMG.ARW", nil, "")
	_, _ = ApplyOp(a, TruthOp{HLC: "ff-0000-devA", Device: "devA", AssetKey: "x", Field: "rating", Value: 3})
	_, _ = ApplyOp(a, TruthOp{HLC: "019a2f001000-0001-devA", Device: "devA", AssetKey: "x", Field: "variant", Value: map[string]any{
		"id": "v1", "name": "Look 1", "recipe": map[string]any{"exposure": 0.5},
	}})

	b := Empty("IMG.ARW", nil, "")
	_, _ = ApplyOp(b, TruthOp{HLC: "100-0000-devB", Device: "devB", AssetKey: "x", Field: "rating", Value: 5})
	// Tombstone with an OLDER HLC must still dominate the variant forever!
	_, _ = ApplyOp(b, TruthOp{HLC: "019a2f000001-0001-devB", Device: "devB", AssetKey: "x", Field: "variant", Value: map[string]any{
		"id": "v1", "tombstone": true,
	}})

	c := Empty("IMG.ARW", nil, "")
	_, _ = ApplyOp(c, TruthOp{HLC: "019a2f002000-0001-devC", Device: "devC", AssetKey: "x", Field: "keywords", Value: []any{"night", "hdr"}})

	// Commutativity: merge(a, b) == merge(b, a)
	ab, _ := Merge(a, b, MergeOptions{})
	ba, _ := Merge(b, a, MergeOptions{})
	abS, _ := Serialize(ab)
	baS, _ := Serialize(ba)
	if string(abS) != string(baS) {
		t.Fatalf("commutativity failed:\n%s\nvs\n%s", abS, baS)
	}

	// Numeric HLC check: 100 > ff so rating must be 5.
	rating, _, _, _, _ := ExtractVerdicts(ab)
	if rating != 5 {
		t.Fatalf("expected numeric HLC winner rating=5, got %d", rating)
	}

	// Tombstone dominance check.
	if len(ab.Variants) != 1 || !ab.Variants[0].Tombstone {
		t.Fatalf("expected tombstoned variant v1, got %+v", ab.Variants)
	}

	// Associativity: merge(merge(a, b), c) == merge(a, merge(b, c))
	abC, _ := Merge(ab, c, MergeOptions{})
	bc, _ := Merge(b, c, MergeOptions{})
	aBC, _ := Merge(a, bc, MergeOptions{})
	s1, _ := Serialize(abC)
	s2, _ := Serialize(aBC)
	if string(s1) != string(s2) {
		t.Fatalf("associativity failed:\n%s\nvs\n%s", s1, s2)
	}

	// Idempotence: merge(abC, abC) == abC
	idem, _ := Merge(abC, abC, MergeOptions{})
	s3, _ := Serialize(idem)
	if string(s1) != string(s3) {
		t.Fatalf("idempotence failed")
	}
}

func TestStoreAtomicWriteLegacyPrecedenceAndQuarantine(t *testing.T) {
	dir := t.TempDir()
	sidecarRoot := filepath.Join(dir, ".hdrtoys")
	st := NewStore()

	// Legacy .hdrt.json fallback is read when canonical .edits doesn't exist.
	legacyPath := filepath.Join(dir, "SUB", "A.JPG.hdrt.json")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyBody := `{"hdrt":1,"asset":{"file":"A.JPG"},"fields":{"rating":{"hlc":"019a2f001000-0001-devA","v":4}}}`
	if err := os.WriteFile(legacyPath, []byte(legacyBody), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := st.Read(dir, sidecarRoot, "SUB/A.JPG")
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	if res.Sidecar == nil || res.SourcePath != legacyPath {
		t.Fatalf("expected legacy sidecar from %s, got %+v", legacyPath, res)
	}

	// Update writes canonically inside .hdrtoys/SUB/A.JPG.edits (leaving legacy untouched).
	_, changed, err := st.Update(dir, sidecarRoot, "SUB/A.JPG", nil, "", func(sc *Sidecar) (bool, error) {
		return ApplyOp(sc, TruthOp{
			HLC:      "019a2f002000-0001-devB",
			Device:   "devB",
			AssetKey: "SUB/A.JPG",
			Field:    "flag",
			Value:    "pick",
		})
	})
	if err != nil || !changed {
		t.Fatalf("update failed: changed=%v err=%v", changed, err)
	}
	canonicalPath := filepath.Join(sidecarRoot, "SUB", "A.JPG.edits")
	if _, err := os.Stat(canonicalPath); err != nil {
		t.Fatalf("expected canonical sidecar at %s: %v", canonicalPath, err)
	}

	// Corrupt canonical .edits must be quarantined, NOT masked by legacy fallback.
	if err := os.WriteFile(canonicalPath, []byte("{corrupt json"), 0o644); err != nil {
		t.Fatal(err)
	}
	qRes, err := st.Read(dir, sidecarRoot, "SUB/A.JPG")
	if err != nil {
		t.Fatalf("read corrupt: %v", err)
	}
	if qRes.Sidecar != nil {
		t.Fatalf("corrupt canonical sidecar must not fall back to legacy silently")
	}
	if qRes.Quarantined == "" {
		t.Fatalf("expected quarantined path")
	}
	if _, err := os.Stat(qRes.Quarantined); err != nil {
		t.Fatalf("expected quarantined file to exist: %v", err)
	}
}

// A non-browser client may post a rating as the literal 5.0. The file is
// rewritten as 5, so the index projection must read the same value.
func TestExtractVerdictsAcceptsIntegerValuedRatingLiterals(t *testing.T) {
	for literal, want := range map[string]int{"5": 5, "5.0": 5, "5e0": 5, "0.0": 0, "4.5": 0, "-1": 0, "1e999": 0} {
		sc, err := Parse([]byte(`{"hdrt":1,"document":{"role":"truth"},"asset":{"file":"a.jpg"},"fields":{"rating":{"hlc":"019a2f0012ab-0001-devA","v":` + literal + `}}}`))
		if err != nil {
			t.Fatalf("parse rating %s: %v", literal, err)
		}
		if rating, _, _, _, _ := ExtractVerdicts(sc); rating != want {
			t.Errorf("rating literal %s projected as %d, want %d", literal, rating, want)
		}
	}
}
