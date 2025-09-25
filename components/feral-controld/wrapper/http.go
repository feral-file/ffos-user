//nolint:gosec
package wrapper

import (
	go_io "io"
	go_http "net/http"
	"time"
)

//go:generate mockgen -source=http.go -destination=../mocks/http.go -package=mocks -mock_names=HTTP=MockHTTP
type HTTP interface {
	Get(url string) (*go_http.Response, error)
	Post(url string, contentType string, body go_io.Reader) (*go_http.Response, error)
}

type http struct {
	client *go_http.Client
}

func NewHTTP() HTTP {
	return http{}
}

func NewHTTPWithTimeout(timeout time.Duration) HTTP {
	return http{
		client: &go_http.Client{
			Timeout: timeout,
		},
	}
}

func (h http) Get(url string) (*go_http.Response, error) {
	return h.client.Get(url)
}

func (h http) Post(url string, contentType string, body go_io.Reader) (*go_http.Response, error) {
	return h.client.Post(url, contentType, body)
}
