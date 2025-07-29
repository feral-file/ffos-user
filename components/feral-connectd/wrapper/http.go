//nolint:gosec
package wrapper

import go_http "net/http"

//go:generate mockgen -source=http.go -destination=../mocks/http.go -package=mocks -mock_names=HTTP=MockHTTP
type HTTP interface {
	Get(url string) (*go_http.Response, error)
}

type http struct{}

func NewHTTP() HTTP {
	return http{}
}

func (h http) Get(url string) (*go_http.Response, error) {
	return go_http.Get(url)
}
