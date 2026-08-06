package offlinecache

import (
	"context"
	go_io "io"
	go_http "net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// hangingHTTPClient's Do blocks until ctx is done, mimicking a kiosk
// DevTools /json endpoint that accepts the TCP connection but never
// completes the response — the scenario defaultDialTimeout's doc
// describes for main.go's onConnect -> AttachOnReconnect call chain.
type hangingHTTPClient struct{}

func (hangingHTTPClient) NewRequest(method, url string, body go_io.Reader) (*go_http.Request, error) {
	return go_http.NewRequest(method, url, nil)
}
func (hangingHTTPClient) Do(req *go_http.Request) (*go_http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}
func (hangingHTTPClient) Get(url string) (*go_http.Response, error) { return nil, nil }
func (hangingHTTPClient) Post(url, contentType string, body go_io.Reader) (*go_http.Response, error) {
	return nil, nil
}

var _ wrapper.HTTPClient = hangingHTTPClient{}

// TestDialPageSession_HangingEndpointUnblocksViaInternalCeiling is the
// regression test for the reconnect-attach hang hazard: main.go's CDP
// onConnect hook calls AttachOnReconnect (-> DialPageSession)
// with the daemon's own long-lived lifetime context, so a kiosk DevTools
// endpoint that never completes the /json discovery request must not
// wedge this call (and therefore the connect-loop goroutine driving all
// future reconnects) forever.
func TestDialPageSession_HangingEndpointUnblocksViaInternalCeiling(t *testing.T) {
	start := time.Now()
	_, err := dialPageSessionWithTimeout(
		context.Background(), "http://127.0.0.1:9222",
		hangingHTTPClient{}, nil, wrapper.NewJSON(), wrapper.NewIO(),
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)),
		100*time.Millisecond,
	)
	require.Error(t, err, "dialing a hanging endpoint must fail, not block forever")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second,
		"dial must return once its internal ceiling elapses, not hang indefinitely")
}
