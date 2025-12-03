package wrapper

import go_filepath "path/filepath"

//go:generate mockgen -source=filepath.go -destination=../mocks/filepath.go -package=mocks -mock_names=Filepath=MockFilepath
type Filepath interface {
	Glob(pattern string) ([]string, error)
}

type filepath struct{}

func NewFilepath() Filepath {
	return &filepath{}
}

func (fp *filepath) Glob(pattern string) ([]string, error) {
	return go_filepath.Glob(pattern)
}
