//nolint:gosec
package wrapper

import go_io "io"

//go:generate mockgen -source=io.go -destination=../mocks/io.go -package=mocks -mock_names=IO=MockIO
type IO interface {
	ReadAll(r go_io.Reader) ([]byte, error)
}

type io struct{}

func NewIO() IO {
	return io{}
}

func (i io) ReadAll(r go_io.Reader) ([]byte, error) {
	return go_io.ReadAll(r)
}
