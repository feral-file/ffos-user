//nolint:gosec
package wrapper

import "time"

//go:generate mockgen -source=clock.go -destination=../mocks/clock.go -package=mocks -mock_names=Clock=MockClock
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	NewTicker(d time.Duration) *time.Ticker
}

type clock struct{}

func NewClock() Clock {
	return &clock{}
}

func (t *clock) Now() time.Time {
	return time.Now()
}

func (t *clock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (t *clock) NewTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}
