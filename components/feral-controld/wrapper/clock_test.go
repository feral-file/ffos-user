package wrapper

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClock_SleepContext_AlreadyCanceled(t *testing.T) {
	c := NewClock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.SleepContext(ctx, time.Hour)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClock_SleepContext_Completes(t *testing.T) {
	c := NewClock()
	ctx := context.Background()
	start := time.Now()
	err := c.SleepContext(ctx, 50*time.Millisecond)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 45*time.Millisecond)
}

func TestClock_SleepContext_DeadlineWinsBeforeTimer(t *testing.T) {
	c := NewClock()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(30*time.Millisecond))
	defer cancel()
	start := time.Now()
	err := c.SleepContext(ctx, time.Hour)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}
