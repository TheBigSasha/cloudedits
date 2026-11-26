package sidecar

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thebigsasha/cloudedits/internal/hlc"
)

// referenceVector is one case frozen from the browser editor's sidecar
// implementation: the same inputs driven through that implementation, with the
// bytes it produced. This package must reproduce every one byte-for-byte.
type referenceVector struct {
	Name        string            `json:"name"`
	Op          string            `json:"op"`
	Text        *string           `json:"text"`
	Asset       map[string]any    `json:"asset"`
	Ops         []json.RawMessage `json:"ops"`
	A           string            `json:"a"`
	B           string            `json:"b"`
	S           string            `json:"s"`
	Millis      uint64            `json:"millis"`
	Counter     uint32            `json:"counter"`
	Device      string            `json:"device"`
	Want        string            `json:"want"`
	WantChanged []bool            `json:"wantChanged"`
	WantRescue  string            `json:"wantRescue"`
}

func loadReferenceVectors(t *testing.T) []referenceVector {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "testdata", "reference_vectors", "vectors.jsonl"))
	if err != nil {
		t.Fatalf("open reference vectors: %v", err)
	}
	defer func() { _ = f.Close() }()

	var vectors []referenceVector
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var v referenceVector
		if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
			t.Fatalf("decode reference vector %d: %v", len(vectors)+1, err)
		}
		vectors = append(vectors, v)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read reference vectors: %v", err)
	}
	if len(vectors) == 0 {
		t.Fatal("no reference vectors loaded")
	}
	return vectors
}

func runReferenceVector(v referenceVector) (out, rescue string, changed []bool, err error) {
	serialize := func(s *Sidecar) (string, error) {
		b, serr := Serialize(s)
		return string(b), serr
	}
	switch v.Op {
	case "roundtrip":
		s, perr := Parse([]byte(*v.Text))
		if perr != nil {
			return "", "", nil, perr
		}
		out, err = serialize(s)
	case "apply":
		var s *Sidecar
		if v.Text != nil {
			if s, err = Parse([]byte(*v.Text)); err != nil {
				return "", "", nil, err
			}
		} else {
			file, _ := v.Asset["file"].(string)
			var size *int64
			if b, ok := v.Asset["bytes"].(float64); ok {
				n := int64(b)
				size = &n
			}
			s = Empty(file, size, "")
		}
		for _, raw := range v.Ops {
			var op TruthOp
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err = dec.Decode(&op); err != nil {
				return "", "", nil, err
			}
			opChanged, aerr := ApplyOp(s, op)
			if aerr != nil {
				return "", "", nil, aerr
			}
			changed = append(changed, opChanged)
		}
		out, err = serialize(s)
	case "merge":
		a, perr := Parse([]byte(v.A))
		if perr != nil {
			return "", "", nil, perr
		}
		b, perr := Parse([]byte(v.B))
		if perr != nil {
			return "", "", nil, perr
		}
		plain, merr := Merge(a, b, MergeOptions{})
		if merr != nil {
			return "", "", nil, merr
		}
		if out, err = serialize(plain); err != nil {
			return "", "", nil, err
		}
		rescued, merr := Merge(a, b, MergeOptions{RescueConflictingRecipes: true})
		if merr != nil {
			return "", "", nil, merr
		}
		rescue, err = serialize(rescued)
	case "hlcParse":
		p, perr := hlc.Parse(v.S)
		if perr != nil {
			return "", "", nil, perr
		}
		b, _ := json.Marshal([]string{fmt.Sprint(p.Millis), fmt.Sprint(p.Counter), p.Device})
		out = string(b)
	case "hlcCompare":
		cmp, cerr := hlc.Compare(v.A, v.B)
		if cerr != nil {
			return "", "", nil, cerr
		}
		out = fmt.Sprint(cmp)
	case "hlcFormat":
		out = hlc.Format(v.Millis, v.Counter, v.Device)
	default:
		err = fmt.Errorf("unknown vector op %q", v.Op)
	}
	return out, rescue, changed, err
}

// TestReferenceVectorsByteIdentity is the cross-implementation conformance
// gate: parse -> serialize, op application, merge, conflict rescue, and HLC
// parse/compare/format must match the browser editor's bytes exactly.
func TestReferenceVectorsByteIdentity(t *testing.T) {
	counts := map[string]int{}
	for _, v := range loadReferenceVectors(t) {
		counts[v.Op]++
		out, rescue, changed, err := runReferenceVector(v)
		if err != nil {
			t.Errorf("%s: reference accepts this input, got error: %v", v.Name, err)
			continue
		}
		if out != v.Want {
			t.Errorf("%s: output differs from reference\n--- want ---\n%q\n--- got ---\n%q", v.Name, v.Want, out)
		}
		// Conflict rescue is defined for two truth copies; vectors that merge a
		// portable document carry no rescue expectation.
		if v.WantRescue != "" && rescue != v.WantRescue {
			t.Errorf("%s: conflict-rescue merge differs from reference\n--- want ---\n%q\n--- got ---\n%q", v.Name, v.WantRescue, rescue)
		}
		if v.WantChanged != nil && !slices.Equal(changed, v.WantChanged) {
			t.Errorf("%s: per-op changed flags = %v, reference = %v", v.Name, changed, v.WantChanged)
		}
	}
	for _, op := range []string{"roundtrip", "apply", "merge", "hlcParse", "hlcCompare", "hlcFormat"} {
		if counts[op] == 0 {
			t.Errorf("reference vectors contain no %q cases", op)
		}
	}
}
