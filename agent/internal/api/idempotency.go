package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// idemRecord is the original accepted response for an Idempotency-Key.
type idemRecord struct {
	Fingerprint string
	Status      int
	Body        []byte
	JobID       string
	StoredAt    time.Time
}

// IdempotencyStore remembers accepted writes for this process.
// Keys are not durable across restarts in this slice. Entries older than
// DefaultRetention are dropped.
type IdempotencyStore struct {
	mu  sync.Mutex
	m   map[string]idemRecord
	ttl time.Duration
	now func() time.Time
}

// NewIdempotencyStore returns an empty store.
func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{
		m:   map[string]idemRecord{},
		ttl: DefaultRetention,
		now: time.Now,
	}
}

// Lookup returns a previous record. found is false when key is new or expired.
func (s *IdempotencyStore) Lookup(key string) (idemRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	rec, ok := s.m[key]
	return rec, ok
}

// Remember stores the original accepted response. It does not replace a key.
func (s *IdempotencyStore) Remember(key string, rec idemRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if _, ok := s.m[key]; ok {
		return
	}
	if rec.StoredAt.IsZero() {
		rec.StoredAt = s.now()
	}
	s.m[key] = rec
}

func (s *IdempotencyStore) sweepLocked() {
	if s.ttl <= 0 {
		return
	}
	now := s.now()
	for key, rec := range s.m {
		if now.Sub(rec.StoredAt) > s.ttl {
			delete(s.m, key)
		}
	}
}

func fingerprint(method, path, ifMatch string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(ifMatch))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
