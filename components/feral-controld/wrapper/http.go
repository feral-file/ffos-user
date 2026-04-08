//nolint:gosec
package wrapper

import (
	"context"
	go_io "io"
	go_http "net/http"
	"time"
)

const (
	// HTTPClientTimeout is the default request timeout for all HTTP clients.
	// Prevents slow or stalled endpoints from blocking indefinitely.
	HTTPClientTimeout = 30 * time.Second
)

//go:generate mockgen -source=http.go -destination=../mocks/http.go -package=mocks -mock_names=HTTPClient=MockHTTPClient
type HTTPClient interface {
	NewRequest(method string, url string, body go_io.Reader) (*go_http.Request, error)
	Do(req *go_http.Request) (*go_http.Response, error)
	Get(url string) (*go_http.Response, error)
	Post(url string, contentType string, body go_io.Reader) (*go_http.Response, error)
}

type httpClient struct {
	client *go_http.Client
}

func NewHTTPClient() HTTPClient {
	return httpClient{
		client: &go_http.Client{
			Timeout: HTTPClientTimeout,
		},
	}
}

func (h httpClient) NewRequest(method string, url string, body go_io.Reader) (*go_http.Request, error) {
	return go_http.NewRequest(method, url, body)
}

func (h httpClient) Do(req *go_http.Request) (*go_http.Response, error) {
	return h.client.Do(req)
}

func (h httpClient) Get(url string) (*go_http.Response, error) {
	return h.client.Get(url)
}

func (h httpClient) Post(url string, contentType string, body go_io.Reader) (*go_http.Response, error) {
	return h.client.Post(url, contentType, body)
}

//go:generate mockgen -source=http.go -destination=../mocks/http.go -package=mocks -mock_names=HTTPServer=MockHTTPServer
type HTTPServer interface {
	Handler() go_http.Handler
	ListenAndServe() error
	Shutdown(ctx context.Context) error
}

type httpServer struct {
	server *go_http.Server
}

func NewHTTPServer(server *go_http.Server) HTTPServer {
	return &httpServer{server: server}
}

func (h httpServer) Handler() go_http.Handler {
	return h.server.Handler
}

func (h httpServer) ListenAndServe() error {
	return h.server.ListenAndServe()
}

func (h httpServer) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}
