// Package runstate records which cloudedits process owns a data directory and
// what that process last reported about itself.
//
// One data directory holds one tailnet identity (tsnet-state), one index and
// one thumbnail cache, so at most one process may use it at a time: two tsnet
// nodes sharing a node key knock each other off the tailnet. The owner holds
// an exclusive OS file lock on <dataDir>/serve.lock for as long as it runs; the
// kernel drops the lock when the process exits, however it exits, so a held
// lock is proof that the owner is alive.
//
// Alongside the lock the server keeps <dataDir>/serve-status.json up to date
// with its state and the results of its own reachability checks. Commands that
// cannot reach the tailnet themselves (doctor, the setup wizard) read it.
package runstate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	lockName   = "serve.lock"
	statusName = "serve-status.json"
)

// ErrLocked reports that another live process owns the data directory.
var ErrLocked = errors.New("another cloudedits process is using this data directory")

// Lock is an exclusive claim on a data directory.
type Lock struct {
	f *os.File
}

// Acquire claims dataDir for this process without blocking. It returns
// ErrLocked (wrapped with the owner's PID when known) if another live process
// holds it.
func Acquire(dataDir string) (*Lock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataDir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			if st, ok := ReadStatus(dataDir); ok && st.PID > 0 {
				return nil, fmt.Errorf("%w (pid %d)", ErrLocked, st.PID)
			}
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
	}
	return &Lock{f: f}, nil
}

// Release gives up the claim. It is safe to call on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unlockFile(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}

// Held reports whether a live process currently owns dataDir.
func Held(dataDir string) bool {
	l, err := Acquire(dataDir)
	if err != nil {
		return errors.Is(err, ErrLocked)
	}
	_ = l.Release()
	return false
}

// State is the server's lifecycle state.
type State string

const (
	// StateStarting: the process owns the data dir and is bringing up its listener.
	StateStarting State = "starting"
	// StateServing: the listener is accepting requests.
	StateServing State = "serving"
	// StateStopped: the process exited cleanly.
	StateStopped State = "stopped"
	// StateFailed: the process exited because it could not serve; see Error.
	StateFailed State = "failed"
)

// Check is the outcome of one self-check the server ran on itself.
type Check struct {
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Status is what the owning process last reported.
type Status struct {
	PID         int       `json:"pid"`
	Version     string    `json:"version,omitempty"`
	State       State     `json:"state"`
	Mode        string    `json:"mode,omitempty"` // "tsnet" | "listen"
	URL         string    `json:"url,omitempty"`
	ListenAddr  string    `json:"listenAddr,omitempty"`
	FQDN        string    `json:"fqdn,omitempty"`
	TailscaleIP string    `json:"tailscaleIp,omitempty"`
	Error       string    `json:"error,omitempty"`
	ExitCode    int       `json:"exitCode,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// IndexedAssets is the asset count when the server started listening
	// and again after each full scan.
	IndexedAssets int `json:"indexedAssets"`
	// MagicDNS is whether the node's own tailnet resolver answers for FQDN
	// with the node's address (what other tailnet devices will resolve).
	MagicDNS *Check `json:"magicDns,omitempty"`
	// HTTPS is whether a TLS handshake with FQDN through the tailnet
	// completed with a certificate valid for FQDN.
	HTTPS *Check `json:"https,omitempty"`
	// CertNotAfter is the expiry of the certificate the handshake saw.
	CertNotAfter *time.Time `json:"certNotAfter,omitempty"`
}

// StatusPath returns the status file path for dataDir.
func StatusPath(dataDir string) string { return filepath.Join(dataDir, statusName) }

// WriteStatus atomically replaces the status file. Only the lock owner
// should call it.
func WriteStatus(dataDir string, st Status) error {
	st.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dataDir, ".serve-status."+hex.EncodeToString(rnd[:])+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, StatusPath(dataDir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadStatus returns the last status written for dataDir, if any.
func ReadStatus(dataDir string) (Status, bool) {
	raw, err := os.ReadFile(StatusPath(dataDir))
	if err != nil {
		return Status{}, false
	}
	var st Status
	if json.Unmarshal(raw, &st) != nil {
		return Status{}, false
	}
	return st, true
}

// Live is the status of dataDir as other processes should see it: the
// recorded status, corrected for an owner that died without saying so.
type Live struct {
	Status
	// Running is true while a process holds the data directory lock.
	Running bool `json:"running"`
	// Recorded is false when no status file exists.
	Recorded bool `json:"recorded"`
}

// Observe reads the status of dataDir and checks whether its owner is alive.
// A status that claims to be starting or serving while no process holds the
// lock belonged to a process that was killed; it is reported as failed.
func Observe(dataDir string) Live {
	st, ok := ReadStatus(dataDir)
	live := Live{Status: st, Recorded: ok, Running: Held(dataDir)}
	if ok && !live.Running && (st.State == StateStarting || st.State == StateServing) {
		live.State = StateFailed
		if live.Error == "" {
			live.Error = fmt.Sprintf("the server (pid %d) stopped without recording why", st.PID)
		}
	}
	return live
}
