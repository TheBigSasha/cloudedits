package auth

import (
	"testing"
	"time"
)

// The session table is bounded even when tokens are minted and never used.
func TestSessionTableIsBounded(t *testing.T) {
	sm := NewSessionManager(time.Hour)
	p := OwnerPrincipal("tailnet:alice@example.com")
	var last string
	for i := 0; i < maxSessions+500; i++ {
		last, _ = sm.Issue(p)
	}
	sm.mu.RLock()
	n := len(sm.sessions)
	sm.mu.RUnlock()
	if n > maxSessions {
		t.Fatalf("session table holds %d entries, cap is %d", n, maxSessions)
	}
	if _, ok := sm.Validate(last); !ok {
		t.Fatal("the most recently issued session must stay valid")
	}

	// Expired sessions are reclaimed in bulk once the table fills up.
	short := NewSessionManager(time.Millisecond)
	for i := 0; i < maxSessions; i++ {
		short.Issue(p)
	}
	time.Sleep(5 * time.Millisecond)
	fresh, _ := short.Issue(p)
	short.mu.RLock()
	n = len(short.sessions)
	short.mu.RUnlock()
	if n != 1 {
		t.Fatalf("expected only the fresh session after expiry sweep, table holds %d", n)
	}
	_ = fresh
}
