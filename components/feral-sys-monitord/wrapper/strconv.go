package wrapper

import go_strconv "strconv"

//go:generate mockgen -source=strconv.go -destination=../mocks/strconv.go -package=mocks -mock_names=Strconv=MockStrconv
type Strconv interface {
	ParseInt(s string, base int, bitSize int) (int64, error)
	ParseFloat(s string, bitSize int) (float64, error)
	Atoi(s string) (int, error)
}

type strconv struct{}

func NewStrconv() Strconv {
	return &strconv{}
}

func (sc *strconv) ParseInt(s string, base int, bitSize int) (int64, error) {
	return go_strconv.ParseInt(s, base, bitSize)
}

func (sc *strconv) ParseFloat(s string, bitSize int) (float64, error) {
	return go_strconv.ParseFloat(s, bitSize)
}

func (sc *strconv) Atoi(s string) (int, error) {
	return go_strconv.Atoi(s)
}
