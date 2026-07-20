package offlinecache

import (
	"context"
	"fmt"
	go_http "net/http"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

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
