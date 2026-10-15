package api

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// idemRecord is the original accepted response for an Idempotency-Key.
type idemRecord struct {
	Fingerprint string
	Status      int
	Body        []byte
	JobID       string
}

// IdempotencyStore remembers accepted writes for this process.
// Keys are not durable across restarts in this slice.
type IdempotencyStore struct {
	mu sync.Mutex
	m  map[string]idemRecord
}

// NewIdempotencyStore returns an empty store.
func NewIdempotencyStore() *IdempotencyStore {
	return &IdempotencyStore{m: map[string]idemRecord{}}
}

// Lookup returns a previous record. found is false when key is new.
func (s *IdempotencyStore) Lookup(key string) (idemRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[key]
	return rec, ok
}

// Remember stores the original accepted response. It does not replace a key.
func (s *IdempotencyStore) Remember(key string, rec idemRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; ok {
		return
	}
	s.m[key] = rec
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
