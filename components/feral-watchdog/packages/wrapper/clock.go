//nolint:gosec
package wrapper

import "time"

//go:generate mockgen -source=clock.go -destination=../mocks/mock_clock.go -package=mocks -mock_names=ClockInterface=MockClock
type ClockInterface interface {
	Now() time.Time
	Sleep(d time.Duration)
	NewTicker(d time.Duration) *time.Ticker
}

type Clock struct{}

func NewClock() ClockInterface {
	return &Clock{}
}

func (t *Clock) Now() time.Time {
	return time.Now()
}

func (t *Clock) Sleep(d time.Duration) {
	time.Sleep(d)
}

func (t *Clock) NewTicker(d time.Duration) *time.Ticker {
	return time.NewTicker(d)
}
