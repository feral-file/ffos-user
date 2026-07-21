package offlinecache

import (
	"context"
	"fmt"
	go_http "net/http"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// defaultDialTimeout bounds the whole targets-fetch + websocket-dial
// sequence below, on top of whatever deadline the caller's ctx already
// carries. main.go's CDP onConnect hook calls kioskReplay.AttachOnReconnect
// (-> here) with the daemon's own long-lived lifetime context, so without
// this ceiling, a kiosk DevTools HTTP endpoint
// or websocket upgrade that hangs (accepts the connection but never
// completes it) would wedge that onConnect callback forever, which runs
// synchronously on cdp.CDP's connect-loop goroutine and would therefore
// also stall all future reconnect attempts, not just this one dial.
const defaultDialTimeout = 15 * time.Second

// DialPageSession discovers endpoint's first "page" DevTools target (via
// the HTTP /json target-list endpoint every Chromium DevTools instance
// exposes) and dials an event-driven CDPSession to it. This is shared by
// capture (against the headless downloader's Chromium) and replay
// (against the kiosk's Chromium): both need the same target-discovery
// dance, just against different endpoints/lifecycles, so it is kept as
// one seam rather than duplicated per caller. It mirrors cdp.go's
// dialPageTarget discovery but is intentionally separate — it must not
// inherit that client's synchronous request/reply model or
// reconnect-supervisor lifecycle.
func DialPageSession(
	ctx context.Context,
	endpoint string,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	logger *zap.Logger,
) (CDPSession, error) {
	return dialPageSessionWithTimeout(ctx, endpoint, httpClient, dialer, jsonWrapper, ioWrapper, logger, defaultDialTimeout)
}

// dialPageSessionWithTimeout is DialPageSession's actual implementation,
// with an injectable dialTimeout so tests can pin the wedged-endpoint
// ceiling behavior without waiting out the real production timeout —
// mirrors newCDPSessionWithTimeout's same pattern in cdpsession.go.
func dialPageSessionWithTimeout(
	ctx context.Context,
	endpoint string,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	logger *zap.Logger,
	dialTimeout time.Duration,
) (CDPSession, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	req, err := httpClient.NewRequest(go_http.MethodGet, endpoint+"/json", nil)
	if err != nil {
		return nil, fmt.Errorf("build targets request: %w", err)
	}
	resp, err := httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetch targets: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := ioWrapper.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read targets: %w", err)
	}

	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := jsonWrapper.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("parse targets: %w", err)
	}

	var wsURL string
	for _, t := range targets {
		if t.Type == "page" {
			wsURL = t.WebSocketDebuggerURL
			break
		}
	}
	if wsURL == "" {
		return nil, fmt.Errorf("no page target found at %s", endpoint)
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial page target: %w", err)
	}
	return NewCDPSession(conn, jsonWrapper, logger), nil
}
