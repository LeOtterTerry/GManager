package api

import (
	"sync"
	"time"
)

const (
	// DefaultRateLimit is the number of requests allowed per window per client.
	DefaultRateLimit = 120
	// DefaultRateWindow is the rate-limit window.
	DefaultRateWindow = time.Minute
)

// Limiter is a sliding-window counter. It is per process and per client IP.
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	hits   map[string][]time.Time
}

// NewLimiter returns a limiter. limit must be at least 1.
func NewLimiter(limit int, window time.Duration) *Limiter {
	if limit < 1 {
		limit = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{
		limit:  limit,
		window: window,
		now:    time.Now,
		hits:   map[string][]time.Time{},
	}
}

// Allow reports whether key may proceed. retryAfter is set when it may not.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	now := l.now()
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := l.hits[key]
	kept := make([]time.Time, 0, len(prev)+1)
	for _, hit := range prev {
		if hit.After(cutoff) {
			kept = append(kept, hit)
		}
	}
	if len(kept) >= l.limit {
		wait := l.window - now.Sub(kept[0])
		if wait < time.Second {
			wait = time.Second
		}
		l.hits[key] = kept
		return false, wait
	}
	l.hits[key] = append(kept, now)
	return true, 0
}
