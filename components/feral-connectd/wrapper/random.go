//nolint:gosec
package wrapper

import (
	"math/rand"
	"time"
)

//go:generate mockgen -source=random.go -destination=../mocks/random.go -package=mocks -mock_names=Randomizer=MockRandomizer
type Randomizer interface {
	Intn(n int) int
	Duration(min, max time.Duration) time.Duration
}

type randomizer struct{}

func NewRandomizer() Randomizer {
	return &randomizer{}
}

func (r *randomizer) Intn(n int) int {
	return rand.Intn(n)
}

func (r *randomizer) Duration(min, max time.Duration) time.Duration {
	return time.Duration(rand.Intn(int(max-min))) + min
}
