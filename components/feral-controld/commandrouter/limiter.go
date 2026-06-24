package commandrouter

import (
	"sync"
	"time"
)

// tokenBucket is a simple token-bucket rate limiter. Tokens refill continuously
// at `rate` tokens per second up to `burst` capacity. It is safe for concurrent
// use. The clock is injectable so behavior is deterministic under test.
//
// A rate of 0 means "unlimited" — allow always.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64 // maximum tokens
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(rate float64, burst int, now func() time.Time) *tokenBucket {
	if now == nil {
		now = time.Now
	}
	return &tokenBucket{
		rate:   rate,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   now(),
		now:    now,
	}
}

// allow consumes a single token if one is available, returning true. When the
// limiter is unlimited (rate <= 0) it always returns true.
func (b *tokenBucket) allow() bool {
	if b.rate <= 0 {
		return true
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// limiterSet holds one token bucket per limiter key (a command type or a shared
// group such as "input"). Buckets are created lazily on first use; all command
// types that share a group key therefore share a single bucket.
type limiterSet struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	now     func() time.Time
}

func newLimiterSet(now func() time.Time) *limiterSet {
	if now == nil {
		now = time.Now
	}
	return &limiterSet{
		buckets: make(map[string]*tokenBucket),
		now:     now,
	}
}

// allow reports whether a command governed by policy p (under the given limiter
// key) may proceed.
func (s *limiterSet) allow(key string, p Policy) bool {
	s.mu.Lock()
	bucket, ok := s.buckets[key]
	if !ok {
		bucket = newTokenBucket(p.Rate, p.Burst, s.now)
		s.buckets[key] = bucket
	}
	s.mu.Unlock()

	return bucket.allow()
}
