//nolint:gosec
package wrapper

import (
	"context"
	"time"
)

//go:generate mockgen -source=clock.go -destination=../mocks/clock.go -package=mocks -mock_names=Clock=MockClock
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	// SleepContext waits until d elapses unless ctx is canceled first. Returns ctx.Err() when ctx is done before d.
	SleepContext(ctx context.Context, d time.Duration) error
	NewTicker(d time.Duration) Ticker
}

type clock struct{}

func NewClock() Clock {
	return &clock{}
}

func (t *clock) Now() time.Time {
	// Always return UTC so sleep schedule clock-times (HH:MM strings) are
	// interpreted consistently regardless of the system timezone configured
	// on the device. Callers must treat sleepTime/wakeTime as UTC values.
	return time.Now().UTC()
}

func (t *clock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (t *clock) SleepContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t *clock) NewTicker(d time.Duration) Ticker {
	return &ticker{time.NewTicker(d)}
}

//go:generate mockgen -source=clock.go -destination=../mocks/clock.go -package=mocks -mock_names=Ticker=MockTicker
type Ticker interface {
	Reset(d time.Duration)
	Stop()
	C() <-chan time.Time
}

type ticker struct {
	*time.Ticker
}

func (t *ticker) Reset(d time.Duration) {
	t.Ticker.Reset(d)
}

func (t *ticker) Stop() {
	t.Ticker.Stop()
}

func (t *ticker) C() <-chan time.Time {
	return t.Ticker.C
}
