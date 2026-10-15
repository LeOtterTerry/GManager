package api

import (
	"testing"
	"time"
)

func TestIdempotencyExpires(t *testing.T) {
	s := NewIdempotencyStore()
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.ttl = time.Minute
	s.Remember("k", idemRecord{Fingerprint: "a", Status: 202})
	if _, ok := s.Lookup("k"); !ok {
		t.Fatal("stored key missing")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := s.Lookup("k"); ok {
		t.Fatal("expired key returned")
	}
}

func TestFinishedJobExpires(t *testing.T) {
	r := NewJobRunner(true)
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.ttl = time.Minute
	job := r.Record("start", "default", map[string]bool{"ok": true}, nil)
	if _, ok := r.Get(job.ID); !ok {
		t.Fatal("job missing")
	}
	now = now.Add(2 * time.Minute)
	if _, ok := r.Get(job.ID); ok {
		t.Fatal("expired job returned")
	}
}
