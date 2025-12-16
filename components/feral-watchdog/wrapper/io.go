//nolint:gosec
package wrapper

import go_io "io"

//go:generate mockgen -source=io.go -destination=../mocks/io.go -package=mocks -mock_names=IOInterface=MockIO
type IO interface {
	ReadAll(r go_io.Reader) ([]byte, error)
	Copy(dst go_io.Writer, src go_io.Reader) (written int64, err error)
}

type io struct{}

func NewIO() IO {
	return io{}
}

func (i io) ReadAll(r go_io.Reader) ([]byte, error) {
	return go_io.ReadAll(r)
}

func (i io) Copy(dst go_io.Writer, src go_io.Reader) (written int64, err error) {
	return go_io.Copy(dst, src)
}
