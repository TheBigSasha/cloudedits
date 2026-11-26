package sidecar

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// externalWriterEnv names the canonical sidecar the helper process rewrites.
const externalWriterEnv = "CLOUDEDITS_TEST_EXTERNAL_WRITER_TARGET"

// TestHelperExternalWriter is not a test: it is the body of the second
// process in the cross-process tests below. It does what a browser editor
// with direct file access does to a sidecar: read the file, merge one edit
// into it, and atomically replace it.
func TestHelperExternalWriter(t *testing.T) {
	target := os.Getenv(externalWriterEnv)
	if target == "" {
		t.Skip("helper process body; run by the cross-process tests")
	}
	sc := Empty("A.JPG", nil, "")
	if data, err := os.ReadFile(target); err == nil {
		if sc, err = Parse(data); err != nil {
			t.Fatalf("external writer: parse: %v", err)
		}
	}
	if _, err := ApplyOp(sc, TruthOp{
		HLC: os.Getenv(externalWriterEnv + "_HLC"), Device: "browser", Field: "keywords", Value: []any{"from-browser"},
	}); err != nil {
		t.Fatalf("external writer: apply: %v", err)
	}
	payload, err := Serialize(sc)
	if err != nil {
		t.Fatalf("external writer: serialize: %v", err)
	}
	swap := target + ".crswap"
	if err := os.WriteFile(swap, payload, 0o644); err != nil {
		t.Fatalf("external writer: stage: %v", err)
	}
	if err := os.Rename(swap, target); err != nil {
		t.Fatalf("external writer: replace: %v", err)
	}
}

// runExternalWriter runs the helper as a real second process and waits for
// its write to be complete on disk.
func runExternalWriter(t *testing.T, target, hlc string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExternalWriter$", "-test.count=1")
	cmd.Env = append(os.Environ(), externalWriterEnv+"="+target, externalWriterEnv+"_HLC="+hlc)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external writer process failed: %v\n%s", err, out)
	}
}

// A second process that owns the same folder (a browser editor with direct
// file access) saves an edit while this server is between reading a sidecar
// and renaming its own rewrite over it. The rename must not discard that
// edit: the other process has already told its user it is saved.
func TestUpdateKeepsAWriteAnotherProcessMadeWhileStaging(t *testing.T) {
	folder := t.TempDir()
	root := filepath.Join(folder, ".hdrtoys")
	store := NewStore()
	if _, _, err := store.Update(folder, root, "A.JPG", nil, "", func(sc *Sidecar) (bool, error) {
		return ApplyOp(sc, TruthOp{HLC: "019a2f001000-0001-server", Device: "server", Field: "rating", Value: 1})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	canonical, _ := CanonicalEditsPath(root, "A.JPG")

	// The staged bytes are flushed after the read and before the rename: the
	// other process writes exactly then, once.
	restore := syncFile
	t.Cleanup(func() { syncFile = restore })
	raced := false
	syncFile = func(f *os.File) error {
		if !raced {
			raced = true
			runExternalWriter(t, canonical, "019a2f002000-0001-browser")
		}
		return restore(f)
	}

	mutations := 0
	if _, changed, err := store.Update(folder, root, "A.JPG", nil, "", func(sc *Sidecar) (bool, error) {
		mutations++
		return ApplyOp(sc, TruthOp{HLC: "019a2f003000-0001-server", Device: "server", Field: "flag", Value: "pick"})
	}); err != nil || !changed {
		t.Fatalf("update: changed=%v err=%v", changed, err)
	}
	if !raced {
		t.Fatal("the other process never wrote; the test did not exercise the window")
	}

	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := sc.Fields["flag"].V; got != "pick" {
		t.Errorf("this server's edit: flag = %v, want pick", got)
	}
	if got := extractStringSlice(sc.Fields["keywords"].V); len(got) != 1 || got[0] != "from-browser" {
		t.Errorf("the other process's saved edit was overwritten: keywords = %v, want [from-browser]", sc.Fields["keywords"].V)
	}
	if mutations != 2 {
		t.Errorf("mutate ran %d times, want 2 (once per read of the sidecar)", mutations)
	}
}

// A process that rewrites the sidecar during every attempt cannot be merged
// with: the update gives up without replacing the file and says it can be
// retried, instead of overwriting the other process's latest write.
func TestUpdateGivesUpWithoutOverwritingUnderSustainedContention(t *testing.T) {
	folder := t.TempDir()
	root := filepath.Join(folder, ".hdrtoys")
	store := NewStore()
	if _, _, err := store.Update(folder, root, "A.JPG", nil, "", func(sc *Sidecar) (bool, error) {
		return ApplyOp(sc, TruthOp{HLC: "019a2f001000-0001-server", Device: "server", Field: "rating", Value: 1})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	canonical, _ := CanonicalEditsPath(root, "A.JPG")

	restore := syncFile
	t.Cleanup(func() { syncFile = restore })
	writes := 0
	syncFile = func(f *os.File) error {
		writes++
		theirs := Empty("A.JPG", nil, "")
		if _, err := ApplyOp(theirs, TruthOp{
			HLC: fmt.Sprintf("019a2f009000-%04x-browser", writes), Device: "browser", Field: "rating", Value: 5,
		}); err != nil {
			t.Fatal(err)
		}
		contender, err := Serialize(theirs)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(canonical, contender, 0o644); err != nil {
			t.Fatal(err)
		}
		return restore(f)
	}

	_, _, err := store.Update(folder, root, "A.JPG", nil, "", func(sc *Sidecar) (bool, error) {
		return ApplyOp(sc, TruthOp{HLC: "019a2f003000-0001-server", Device: "server", Field: "flag", Value: "pick"})
	})
	if !errors.Is(err, ErrContended) {
		t.Fatalf("err = %v, want ErrContended", err)
	}
	if writes != maxCommitAttempts {
		t.Errorf("staged %d times, want %d", writes, maxCommitAttempts)
	}
	data, _ := os.ReadFile(canonical)
	sc, perr := Parse(data)
	if perr != nil {
		t.Fatalf("contender's file was damaged: %v", perr)
	}
	if _, has := sc.Fields["flag"]; has {
		t.Error("the update replaced the file although it reported failure")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(root, ".tmp", "*.tmp")); len(leftovers) != 0 {
		t.Errorf("abandoned staging files left behind: %v", leftovers)
	}
}
