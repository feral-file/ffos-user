package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// kioskUserAgent keeps the kiosk Chromium's own User-Agent, read from its
// DevTools endpoint (`<cdp>/json/version`, field "User-Agent"), for the
// media-health probe (mediahealth.Probe.KioskUserAgent): the probe must
// look like the wall to an origin, and some CDNs refuse non-browser
// clients (bits.raster.art: 403 to Go's default agent, 200 to the kiosk;
// office FF1, 2026-11-11). It is the kiosk's ACTUAL agent, never a made-up
// one, and it changes only when Chromium is upgraded, so it is re-read on
// an interval and the last good value is kept through a kiosk restart.
type kioskUserAgent struct {
	versionURL string
	client     *http.Client
	logger     *zap.Logger
	value      atomic.Value // string
}

// kioskUserAgentRefresh is how often the agent is re-read. A Chromium
// upgrade arrives with an image or package update, so the interval only
// bounds how long a probe can lag one; the first read is at start.
const kioskUserAgentRefresh = 10 * time.Minute

func newKioskUserAgent(cdpEndpoint string, logger *zap.Logger) *kioskUserAgent {
	k := &kioskUserAgent{
		versionURL: strings.TrimRight(cdpEndpoint, "/") + "/json/version",
		client:     &http.Client{Timeout: 3 * time.Second},
		logger:     logger,
	}
	k.value.Store("")
	return k
}

// Get returns the last agent read, "" before the first success (the probe
// then keeps the client's default, as before this existed).
func (k *kioskUserAgent) Get() string { return k.value.Load().(string) }

// Run reads the agent at start and every kioskUserAgentRefresh until ctx
// ends. The kiosk may come up after controld, so a failed read is retried
// on a short backoff until the first success.
func (k *kioskUserAgent) Run(ctx context.Context) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if k.read(ctx) {
			wait = kioskUserAgentRefresh
		} else if k.Get() == "" {
			wait = 15 * time.Second
		} else {
			wait = kioskUserAgentRefresh
		}
	}
}

func (k *kioskUserAgent) read(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.versionURL, nil)
	if err != nil {
		return false
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var v struct {
		UserAgent string `json:"User-Agent"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.UserAgent == "" {
		return false
	}
	if v.UserAgent != k.Get() {
		k.value.Store(v.UserAgent)
		k.logger.Info("media health: probe sends the kiosk's User-Agent", zap.String("user_agent", v.UserAgent))
	}
	return true
}
