//nolint:gosec
package wrapper

import go_math "math"

//go:generate mockgen -source=math.go -destination=../mocks/math.go -package=mocks -mock_names=Math=MockMath
type Math interface {
	Sqrt(x float64) float64
	Max(x, y float64) float64
	Min(x, y float64) float64
}

type math struct{}

func NewMath() Math {
	return &math{}
}

func (m math) Sqrt(x float64) float64 {
	return go_math.Sqrt(x)
}

func (m math) Max(x, y float64) float64 {
	return go_math.Max(x, y)
}

func (m math) Min(x, y float64) float64 {
	return go_math.Min(x, y)
}
