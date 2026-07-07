package commandrouter

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTokenBucket_BurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }

	// 1 token/sec, capacity 2.
	b := newTokenBucket(1, 2, clock)

	// Burst capacity allows the first two immediately.
	assert.True(t, b.allow(), "first request within burst")
	assert.True(t, b.allow(), "second request within burst")
	assert.False(t, b.allow(), "third request exceeds burst")

	// After half a second, no whole token has refilled yet.
	now = now.Add(500 * time.Millisecond)
	assert.False(t, b.allow(), "still no token after 0.5s")

	// After a full second from the last refill, one token is available.
	now = now.Add(500 * time.Millisecond)
	assert.True(t, b.allow(), "one token refilled after 1s")
	assert.False(t, b.allow(), "only one token refilled")
}

func TestTokenBucket_RefillCapsAtBurst(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := newTokenBucket(1, 2, clock)

	// Drain.
	assert.True(t, b.allow())
	assert.True(t, b.allow())
	assert.False(t, b.allow())

	// Idle for a long time; tokens must not accumulate beyond burst.
	now = now.Add(1 * time.Hour)
	assert.True(t, b.allow())
	assert.True(t, b.allow())
	assert.False(t, b.allow(), "accumulated tokens are capped at burst")
}

func TestTokenBucket_Unlimited(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	b := newTokenBucket(0, 0, clock)

	for i := 0; i < 1000; i++ {
		assert.True(t, b.allow(), "rate <= 0 always allows")
	}
}

func TestLimiterSet_SharedGroupKey(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	set := newLimiterSet(clock)

	p := Policy{Group: inputGroup, Rate: 1, Burst: 2}

	// Two distinct command types that share the same group key draw from one
	// bucket: three calls across them exhaust the shared burst of 2.
	assert.True(t, set.allow(inputGroup, p))
	assert.True(t, set.allow(inputGroup, p))
	assert.False(t, set.allow(inputGroup, p), "shared bucket exhausted")
}
