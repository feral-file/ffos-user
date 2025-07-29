//nolint:gosec
package wrapper

import "io"

//go:generate mockgen -source=io.go -destination=../mocks/io.go -package=mocks -mock_names=IOInterface=MockIO
type IOInterface interface {
	ReadAll(r io.Reader) ([]byte, error)
}

type IO struct{}

func NewIO() IO {
	return IO{}
}

func (i IO) ReadAll(r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}
