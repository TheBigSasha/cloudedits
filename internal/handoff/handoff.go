// Package handoff implements a short-lived in-memory UI state handoff store
// (5-minute TTL, compact 8-character base62 IDs) for seamless QR pickup.
package handoff

import (
	"crypto/rand"
	"encoding/json"
	"math/big"
	"sync"
	"time"
)

const (
	defaultTTL = 5 * time.Minute
	base62     = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	idLen      = 8
	maxEntries = 1024
)

// Entry holds one ephemeral handoff payload and its expiration timestamp.
type Entry struct {
	ID        string          `json:"id"`
	Payload   json.RawMessage `json:"payload"`
	ExpiresAt time.Time       `json:"expiresAt"`
}

// Store is a thread-safe TTL map for handoff payloads.
type Store struct {
	mu      sync.Mutex
	ttl     time.Duration
	nowFn   func() time.Time
	entries map[string]Entry
}

// NewStore creates a new handoff Store.
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &Store{
		ttl:     ttl,
		nowFn:   time.Now,
		entries: map[string]Entry{},
	}
}

// Create stores payload and returns a short 8-char base62 ID and expiry time.
func (s *Store) Create(payload json.RawMessage) Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn().UTC()
	s.pruneLocked(now)
	if len(s.entries) >= maxEntries {
		// Evict oldest expiring entry.
		var oldestID string
		var oldestExp time.Time
		for id, e := range s.entries {
			if oldestID == "" || e.ExpiresAt.Before(oldestExp) {
				oldestID = id
				oldestExp = e.ExpiresAt
			}
		}
		if oldestID != "" {
			delete(s.entries, oldestID)
		}
	}

	var id string
	for {
		id = randomBase62(idLen)
		if _, exists := s.entries[id]; !exists {
			break
		}
	}

	cp := make(json.RawMessage, len(payload))
	copy(cp, payload)
	e := Entry{
		ID:        id,
		Payload:   cp,
		ExpiresAt: now.Add(s.ttl),
	}
	s.entries[id] = e
	return e
}

// Get retrieves an unexpired entry by ID.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowFn().UTC()
	e, ok := s.entries[id]
	if !ok {
		return Entry{}, false
	}
	if now.After(e.ExpiresAt) {
		delete(s.entries, id)
		return Entry{}, false
	}
	return e, true
}

// Clear removes all stored handoff entries (used by dev_apis reset).
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = map[string]Entry{}
}

func (s *Store) pruneLocked(now time.Time) {
	for id, e := range s.entries {
		if now.After(e.ExpiresAt) {
			delete(s.entries, id)
		}
	}
}

func randomBase62(n int) string {
	out := make([]byte, n)
	maxVal := big.NewInt(int64(len(base62)))
	for i := range out {
		v, err := rand.Int(rand.Reader, maxVal)
		if err != nil {
			out[i] = base62[i%len(base62)]
			continue
		}
		out[i] = base62[v.Int64()]
	}
	return string(out)
}
