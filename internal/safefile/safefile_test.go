package safefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadRegular(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	data, info, err := ReadRegular(plain, 5)
	if err != nil || !bytes.Equal(data, []byte("hello")) || info.Size() != 5 {
		t.Fatalf("regular file at the limit: %q %v %v", data, info, err)
	}
	if _, _, err := ReadRegular(plain, 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over limit: err=%v, want ErrTooLarge", err)
	}
	if _, _, err := ReadRegular(filepath.Join(dir, "missing"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: err=%v, want os.ErrNotExist", err)
	}
	if _, _, err := ReadRegular(dir, 10); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory: err=%v, want ErrNotRegular", err)
	}

	// A sparse multi-gigabyte file is refused from its size alone.
	huge := filepath.Join(dir, "huge")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(8 << 30); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, _, err := ReadRegular(huge, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("sparse 8 GiB file: err=%v, want ErrTooLarge", err)
	}
}
