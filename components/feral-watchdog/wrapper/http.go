//nolint:gosec
package wrapper

import (
	"context"
	"fmt"
	go_io "io"
	go_http "net/http"
	"time"
)

//go:generate mockgen -source=http.go -destination=../mocks/http.go -package=mocks -mock_names=HTTPClient=MockHTTPClient
type HTTPClient interface {
	Get(url string) (*go_http.Response, error)
	GetWithContext(ctx context.Context, url string) (*go_http.Response, error)
	Post(url string, contentType string, body go_io.Reader) (*go_http.Response, error)
	PostWithContext(ctx context.Context, url string, contentType string, body go_io.Reader) (*go_http.Response, error)
}

type httpClient struct {
	client *go_http.Client
}

func NewHTTPClient(timeout time.Duration) HTTPClient {
	return &httpClient{
		client: &go_http.Client{
			Timeout: timeout,
		},
	}
}

func (h httpClient) Get(url string) (*go_http.Response, error) {
	return h.client.Get(url)
}

func (h httpClient) GetWithContext(ctx context.Context, url string) (*go_http.Response, error) {
	req, err := go_http.NewRequestWithContext(ctx, go_http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return h.client.Do(req)
}

func (h httpClient) Post(url string, contentType string, body go_io.Reader) (*go_http.Response, error) {
	return h.client.Post(url, contentType, body)
}

func (h httpClient) PostWithContext(ctx context.Context, url string, contentType string, body go_io.Reader) (*go_http.Response, error) {
	req, err := go_http.NewRequestWithContext(ctx, go_http.MethodPost, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	return h.client.Do(req)
}
