package refresher

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsTransientPlaylistRefreshError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "dns temporary", err: &net.DNSError{Name: "feed.example", IsTemporary: true}, want: true},
		{name: "dns permanent", err: &net.DNSError{Name: "feed.example", Err: "no such host"}, want: false},
		{name: "url timeout", err: &url.Error{Op: "Get", URL: "https://feed.example", Err: timeoutError{}}, want: true},
		{name: "url permanent", err: &url.Error{Op: "Get", URL: "ftp://feed.example", Err: errors.New("unsupported protocol scheme")}, want: false},
		{name: "http 503", err: fmt.Errorf("fetch playlist failed: 503 Service Unavailable"), want: true},
		{name: "http 429", err: fmt.Errorf("fetch playlist failed: 429 Too Many Requests"), want: true},
		{name: "http 404", err: fmt.Errorf("fetch playlist failed: 404 Not Found"), want: false},
		{name: "json parse", err: errors.New("invalid character 'x' looking for beginning of value"), want: false},
		{name: "dynamic config", err: errors.New("playlist has no dynamic query configuration"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransientPlaylistRefreshError(tc.err))
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
