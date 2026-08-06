//nolint:gosec
package wrapper

import (
	"context"
	go_io "io"
	"net"
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

// NewHTTPClientWithoutTimeout returns a client with NO whole-request timeout.
// http.Client.Timeout covers the entire request INCLUDING the body transfer, so
// the 30s default deterministically kills any large upload on a slow uplink —
// exactly the case (big log archives) where the transfer matters most. Callers
// MUST bound each request themselves via a request context deadline; do not use
// this for ordinary short API calls.
func NewHTTPClientWithoutTimeout() HTTPClient {
	return httpClient{client: &go_http.Client{}}
}

// NewHTTPClientFrom adapts a caller-built *http.Client to this interface.
// It exists so a caller that must control the Transport or the redirect
// policy — offlinecache's source guard is the one today, which enforces
// its reserved-address rules in DialContext so that every redirect hop
// and every re-resolution is checked — can still be injected everywhere a
// wrapper.HTTPClient is expected. The client is used as given: timeouts
// and redirect behavior are entirely the caller's to set.
func NewHTTPClientFrom(client *go_http.Client) HTTPClient {
	return httpClient{client: client}
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
	// Serve runs the server on an already-bound listener (net.Listener),
	// unlike ListenAndServe, which combines binding and serving into one
	// blocking call. Callers that need to know DEFINITIVELY whether a
	// bind succeeded before treating the server as available (see
	// offlinecache.StaticServer's Listen/Serve split and its doc for
	// why net/http's combined ListenAndServe cannot provide that)
	// should net.Listen themselves and call Serve with the result.
	Serve(l net.Listener) error
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

func (h httpServer) Serve(l net.Listener) error {
	return h.server.Serve(l)
}

func (h httpServer) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}
