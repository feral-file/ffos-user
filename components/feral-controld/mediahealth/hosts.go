package mediahealth

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HostLimiter is the per-origin pacing the cast pass and the lookahead
// SHARE. It is one object on the Prober on purpose: a 429 the cast pass
// provokes must slow the lookahead down on the same host, and the two must
// never hold three connections to one origin between them.
//
// Two rules:
//
//   - at most MaxInFlight probes to one host at a time (Acquire blocks the
//     worker until a slot frees, so a 24-item playlist served by one CDN
//     runs at MaxInFlight whatever the pool size — the deliberate trade-off:
//     the pool is sized for a mixed playlist, the origin sees a trickle);
//   - a host that answered 429 or 503 is left alone (Cooldown) for its
//     Retry-After, DefaultHostCooldown when it sent none, never longer than
//     MaxHostCooldown. Sources on a cooled host are not probed and not
//     recorded: they stay unknown (fail-open, no hint) until the cooldown
//     ends, instead of inheriting the one-hour transient hold the probed
//     source itself takes.
//
// Hosts are keyed by the wire URL's hostname (the kiosk rewrite applied).
// An unparseable URL has no host and is not limited: the fetcher's guard
// refuses it anyway.
type HostLimiter struct {
	// MaxInFlight is the per-host in-flight cap (DefaultHostInFlight when 0).
	MaxInFlight int
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	mu       sync.Mutex
	inFlight map[string]int
	cooled   map[string]time.Time
	// released is closed and replaced on every Release so blocked
	// acquirers re-check their host; at most a pool's worth of waiters, so
	// the broadcast is cheap.
	released chan struct{}
}

const (
	// DefaultHostInFlight: two connections per origin is what a browser
	// preloading two images does; an origin that rate-limits by
	// concurrency never sees the pool size.
	DefaultHostInFlight = 2
	// DefaultHostCooldown is the pause after a 429/503 without Retry-After.
	DefaultHostCooldown = time.Minute
	// MaxHostCooldown caps a Retry-After the origin sends: an origin asking
	// to be left alone for a day must not silence its own health checks for
	// a day (the transient hold on the record is capped the same way).
	MaxHostCooldown = time.Hour
)

func (h *HostLimiter) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *HostLimiter) limit() int {
	if h.MaxInFlight > 0 {
		return h.MaxInFlight
	}
	return DefaultHostInFlight
}

// Acquire takes an in-flight slot on host, waiting for one while the host
// is at its cap. It returns ctx's error when the wait is cut short (the
// pass ceiling, shutdown). Every nil return owes one Release.
func (h *HostLimiter) Acquire(ctx context.Context, host string) error {
	if host == "" {
		return nil
	}
	for {
		h.mu.Lock()
		if h.inFlight[host] < h.limit() {
			if h.inFlight == nil {
				h.inFlight = make(map[string]int)
			}
			h.inFlight[host]++
			h.mu.Unlock()
			return nil
		}
		if h.released == nil {
			h.released = make(chan struct{})
		}
		wait := h.released
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

// Release frees a slot taken by Acquire.
func (h *HostLimiter) Release(host string) {
	if host == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inFlight[host] <= 1 {
		delete(h.inFlight, host)
	} else {
		h.inFlight[host]--
	}
	if h.released != nil {
		close(h.released)
		h.released = nil
	}
}

// Cooldown leaves host alone for d (DefaultHostCooldown when d <= 0,
// never more than MaxHostCooldown). A later end wins over an earlier one.
func (h *HostLimiter) Cooldown(host string, d time.Duration) {
	if host == "" {
		return
	}
	if d <= 0 {
		d = DefaultHostCooldown
	}
	if d > MaxHostCooldown {
		d = MaxHostCooldown
	}
	until := h.now().Add(d)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cooled == nil {
		h.cooled = make(map[string]time.Time)
	}
	if until.After(h.cooled[host]) {
		h.cooled[host] = until
	}
}

// Cooled reports whether host is under a cooldown and until when. Expired
// entries are dropped on the way, so the map is bounded by the hosts
// cooled inside the last MaxHostCooldown.
func (h *HostLimiter) Cooled(host string) (time.Time, bool) {
	if host == "" {
		return time.Time{}, false
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	for other, until := range h.cooled {
		if !now.Before(until) {
			delete(h.cooled, other)
		}
	}
	until, ok := h.cooled[host]
	return until, ok
}

// coolsHost says whether a transient status is the origin asking for a
// pause (as opposed to a per-resource auth wall or a bot challenge): only
// 429 and 503 carry that meaning, and only they define Retry-After.
func coolsHost(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

// parseRetryAfter reads a Retry-After header (RFC 9110 §10.2.3: delay
// seconds or an HTTP-date) as a duration from now. ok is false for an
// absent or unparseable header; a date in the past reads as 0, ok.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(header, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(header); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
