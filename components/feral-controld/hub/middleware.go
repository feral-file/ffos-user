package hub

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// MAX_INFLIGHT_REQUESTS bounds the number of hub requests in flight across ALL
// routes at once. It is the HTTP-layer arm of command-storm protection
// (feral-file/ffos-user#208): a LAN flood is shed with 429 at the door before
// any handler decodes a body or reaches the command router, so it cannot pile
// up unbounded goroutines. The per-command token-bucket gate still runs deeper
// in the command router for /api/cast; this is the coarse, route-agnostic cap.
const MAX_INFLIGHT_REQUESTS = 64

// MAX_REQUEST_BODY_BYTES bounds every hub request body via http.MaxBytesReader.
// The in-flight cap alone bounds concurrency, not allocations: without a body
// limit, 64 concurrent LAN casts could each make the provisioning-owning
// daemon buffer an arbitrarily large JSON value while decoding. Sized by the
// largest legitimate envelope: displayPlaylist's `dp1_call` variant carries a
// FULL INLINE DP1 playlist (not just a URL), which scales with artwork count —
// 4 MiB gives even huge playlists generous headroom while capping the
// worst-case transient at 256 MiB instead of unbounded. Same boundary class
// the captive portal carries; the GET routes and the WS upgrade have no body,
// so the reader is inert there. Oversized bodies surface as 413 in handleCast.
const MAX_REQUEST_BODY_BYTES = 4 << 20

// withMiddleware is the SINGLE wrapper every hub route is registered through.
//
// It is deliberately the one chokepoint for cross-cutting concerns on the LAN
// control surface: today an in-flight concurrency limiter (command-storm
// protection) and per-request logging. It is also, by design, the future
// insertion point for LAN authorization (screen-anchored pairing, issue
// #3471). Nothing else may register a hub route directly: any route bypassing
// this wrapper would also bypass the storm cap and the coming authorization
// check, so new cross-cutting behavior belongs here, not in individual
// handlers, and every route MUST go through routes()'s withMiddleware calls.
func (h *hub) withMiddleware(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Shed excess load before doing any work. The slot is released as soon
		// as the handler returns; the WebSocket notification handler returns
		// right after the upgrade (the connection is then serviced by the ws
		// package's own goroutines), so a live WS connection does not hold a
		// slot for its lifetime.
		select {
		case h.reqSlots <- struct{}{}:
			defer func() { <-h.reqSlots }()
		default:
			h.logger.Warn("Hub request rejected: at capacity",
				zap.String("route", route),
				zap.String("remote_addr", r.RemoteAddr),
			)
			http.Error(w, "Too many concurrent requests, slow down", http.StatusTooManyRequests)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, MAX_REQUEST_BODY_BYTES)

		// LAN AUTHORIZATION SEAM (issue #3471): screen-anchored pairing checks
		// go here, guarding every route uniformly, before next is invoked.

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)

		h.logger.Info("Hub request served",
			zap.String("route", route),
			zap.String("method", r.Method),
			zap.String("remote_addr", r.RemoteAddr),
			zap.Int("status", rec.status),
			zap.Duration("duration", time.Since(start)),
		)
	}
}

// statusRecorder wraps http.ResponseWriter to capture the response status for
// request logging. It forwards Hijack so the WebSocket upgrade on
// /api/notification still works through the wrapper.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Hijack lets the WebSocket upgrader take over the connection. Without it the
// gorilla upgrader would fail because the wrapped writer would not satisfy
// http.Hijacker.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	// A hijacked connection reports 101 Switching Protocols by convention.
	r.status = http.StatusSwitchingProtocols
	return hj.Hijack()
}
