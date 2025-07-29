//nolint:gosec
package wrapper

import "net/http"

//go:generate mockgen -source=http.go -destination=../mocks/http.go -package=mocks -mock_names=HTTPInterface=MockHTTP
type HTTPInterface interface {
	Get(url string) (*http.Response, error)
}

type HTTP struct{}

func NewHTTP() HTTPInterface {
	return HTTP{}
}

func (h HTTP) Get(url string) (*http.Response, error) {
	return http.Get(url)
}
