//nolint:gosec
package wrapper

import "time"

//go:generate mockgen -source=clock.go -destination=../mocks/clock.go -package=mocks -mock_names=Clock=MockClock
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	NewTicker(d time.Duration) Ticker
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
