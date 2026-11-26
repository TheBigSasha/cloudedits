package runstate

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLockIsExclusiveAndReleasable(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !Held(dir) {
		t.Fatal("Held = false while the lock is held")
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire: err = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if Held(dir) {
		t.Fatal("Held = true after Release")
	}
	l2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	_ = l2.Release()
}

// A killed owner must not leave the data directory claimed, and its last
// "serving" status must not be reported as live.
func TestKilledOwnerReleasesTheLock(t *testing.T) {
	if os.Getenv("RUNSTATE_HOLDER_DIR") != "" {
		dir := os.Getenv("RUNSTATE_HOLDER_DIR")
		if _, err := Acquire(dir); err != nil {
			os.Exit(7)
		}
		_ = WriteStatus(dir, Status{PID: os.Getpid(), State: StateServing, StartedAt: time.Now()})
		os.Stdout.WriteString("held\n")
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledOwnerReleasesTheLock$")
	cmd.Env = append(os.Environ(), "RUNSTATE_HOLDER_DIR="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("holder did not take the lock: %q", line)
	}
	if live := Observe(dir); !live.Running || live.State != StateServing {
		t.Fatalf("while held: %+v", live)
	}
	if _, err := Acquire(dir); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("Acquire while another process holds it: %v", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	live := Observe(dir)
	if live.Running {
		t.Fatal("lock still held after the owner was killed")
	}
	if live.State != StateFailed || live.Error == "" {
		t.Fatalf("a dead owner's serving status must read as failed: %+v", live)
	}
}

func TestObserveWithoutStatus(t *testing.T) {
	live := Observe(t.TempDir())
	if live.Running || live.Recorded {
		t.Fatalf("empty dir: %+v", live)
	}
}
