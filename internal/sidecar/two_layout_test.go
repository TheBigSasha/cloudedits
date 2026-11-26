package sidecar

import (
	"os"
	"path/filepath"
	"testing"
)

func readTwoLayoutFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "two_layout", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// A folder can hold one asset's truth in both layouts: .hdrtoys/<rel>.edits and
// <rel>.edits beside the photo. The browser editor joins the two on every
// read; testdata/two_layout/merged.edits is the document it produces for this
// pair. Reading only the canonical copy showed rating 2 and no flag while the
// browser showed rating 5 and a pick flag for the same folder.
func TestReadJoinsSidecarFolderAndBesidePhotoCopies(t *testing.T) {
	dir := t.TempDir()
	sidecarRoot := filepath.Join(dir, ".hdrtoys")
	if err := os.MkdirAll(sidecarRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalPath := filepath.Join(sidecarRoot, "IMG_0002.JPG.edits")
	besidePath := filepath.Join(dir, "IMG_0002.JPG.edits")
	besideBytes := readTwoLayoutFixture(t, "beside_photo.edits")
	if err := os.WriteFile(canonicalPath, readTwoLayoutFixture(t, "in_sidecar_folder.edits"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(besidePath, besideBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	want := string(readTwoLayoutFixture(t, "merged.edits"))

	st := NewStore()
	res, err := st.Read(dir, sidecarRoot, "IMG_0002.JPG")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Sidecar == nil || res.SourcePath != canonicalPath || res.MergedFrom != besidePath {
		t.Fatalf("expected a join of %s and %s, got %+v", canonicalPath, besidePath, res)
	}
	got, err := Serialize(res.Sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("two-layout read differs from the browser editor's join\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}

	// A write commits the joined state to the canonical sidecar and leaves the
	// copy beside the photo byte-identical: the photo folder is read-only.
	_, changed, err := st.Update(dir, sidecarRoot, "IMG_0002.JPG", nil, "", func(sc *Sidecar) (bool, error) {
		return ApplyOp(sc, TruthOp{HLC: "019a2f300000-0000-devC", Device: "devC", AssetKey: "x", Field: "rating", Value: 1})
	})
	if err != nil || !changed {
		t.Fatalf("Update: changed=%v err=%v", changed, err)
	}
	afterBeside, err := os.ReadFile(besidePath)
	if err != nil || string(afterBeside) != string(besideBytes) {
		t.Fatalf("copy beside the photo was modified (err=%v)", err)
	}
	written, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := Parse(written)
	if err != nil {
		t.Fatalf("parse written canonical sidecar: %v", err)
	}
	rating, flag, label, _, _ := ExtractVerdicts(onDisk)
	if rating != 1 || flag != "pick" || label != "red" || len(onDisk.Variants) != 2 {
		t.Fatalf("canonical sidecar lost joined state: rating=%d flag=%q label=%q variants=%d", rating, flag, label, len(onDisk.Variants))
	}

	// Re-reading joins the written state with the untouched copy again; the
	// content-derived rescue id makes that a no-op.
	again, err := st.Read(dir, sidecarRoot, "IMG_0002.JPG")
	if err != nil {
		t.Fatal(err)
	}
	againBytes, _ := Serialize(again.Sidecar)
	if string(againBytes) != string(written) {
		t.Fatalf("re-reading after a write is not idempotent\n--- disk ---\n%s\n--- read ---\n%s", written, againBytes)
	}
}

// A shared (portable) edit file that happens to sit beside a photo is not
// folder truth and must never be adopted as the asset's sidecar.
func TestReadIgnoresPortableDocumentBesidePhoto(t *testing.T) {
	dir := t.TempDir()
	sidecarRoot := filepath.Join(dir, ".hdrtoys")
	portable := `{"hdrt":1,"document":{"role":"portable"},"asset":{"file":"A.JPG"},"fields":{"rating":{"hlc":"019a2f001000-0001-devA","v":4}}}`
	if err := os.WriteFile(filepath.Join(dir, "A.JPG.edits"), []byte(portable), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NewStore().Read(dir, sidecarRoot, "A.JPG")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sidecar != nil {
		t.Fatalf("portable document adopted as folder truth from %s", res.SourcePath)
	}
}
