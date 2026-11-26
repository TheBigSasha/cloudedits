//go:build !windows

package safefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadRegularRefusesLinksDevicesAndFIFOs(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"link to file":         plain,
		"link to device":       "/dev/zero",
		"link to missing file": filepath.Join(dir, "nowhere"),
		"link to directory":    dir,
	} {
		link := filepath.Join(dir, "link")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ReadRegular(link, 1<<20); !errors.Is(err, ErrNotRegular) {
			t.Errorf("%s: err=%v, want ErrNotRegular", name, err)
		}
	}

	// A FIFO must be refused immediately instead of blocking on open.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := ReadRegular(fifo, 1<<20)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("fifo: err=%v, want ErrNotRegular", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadRegular blocked on a FIFO")
	}
}
