//nolint:gosec
package wrapper

import "time"

//go:generate mockgen -source=clock.go -destination=../mocks/clock.go -package=mocks -mock_names=Clock=MockClock
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	Sleep(d time.Duration)
	NewTicker(d time.Duration) *time.Ticker
	AfterFunc(d time.Duration, f func()) *time.Timer
}

type clock struct{}

func NewClock() Clock {
	return &clock{}
}

func (c *clock) Now() time.Time {
	return time.Now()
}

func (c *clock) Since(t time.Time) time.Duration {
	return time.Since(t)
}

func (c *clock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (c *clock) NewTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}

func (c *clock) AfterFunc(d time.Duration, f func()) *time.Timer {
	return time.AfterFunc(d, f)
}
