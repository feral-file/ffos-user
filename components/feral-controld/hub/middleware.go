package hub

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	fflogger "github.com/feral-file/ffos-user/components/feral-controld/logger"
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

// countsAsContact names the routes whose traffic evidences a human's app
// talking to the device (see hub.contactObserver). Keyed on the middleware
// route label so the counted set stays legible in one place.
func countsAsContact(route string) bool {
	switch route {
	case "cast", "status", "status_v2":
		return true
	default:
		return false
	}
}

// isLoopbackAddr reports whether an http.Request.RemoteAddr is a loopback
// source. Unparseable addresses count as loopback — the fail direction that
// NEVER fabricates human contact (a fabricated contact defers a recovery
// raise; a missed one merely skips a deferral).
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return ip.IsLoopback()
}

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

		// Web pages may not drive the hub. Until LAN authorization lands
		// (below), the hub trusts every caller on the network, and a browser
		// is the one caller that does not belong to the owner: an artwork
		// playing on this device or any site open in a browser on the same
		// Wi-Fi can send a no-cors POST to /api/cast that the JSON decoder
		// accepts. Browsers always mark cross-site requests with Origin or
		// Sec-Fetch-Site and pages cannot strip them; the app, ff-cli, and
		// the relayer path send neither. The player's log route keeps its own
		// stricter loopback-plus-exact-origin check, so it is exempt here.
		//
		// A page can also reach the hub without either header through DNS
		// rebinding: its own domain is re-pointed at this device, so its
		// requests are same-origin and a GET carries no Origin. Such a request
		// names the attacker's domain in Host, and status hands out the
		// topic ID, so only Host values a public DNS name cannot take are
		// accepted (see isLocalHost).
		if !isLocalHost(r.Host) {
			h.logger.Warn("Hub request rejected: non-local Host",
				zap.String("route", route),
				zap.String("remote_addr", r.RemoteAddr),
				zap.ByteString("host", helper.TruncateBytes([]byte(r.Host), fflogger.MAX_FIELD_LENGTH)),
			)
			http.Error(w, "Host not allowed", http.StatusForbidden)
			return
		}
		if route != playerLogsRoute && isBrowserOriginated(r) {
			h.logger.Warn("Hub request rejected: browser origin",
				zap.String("route", route),
				zap.String("remote_addr", r.RemoteAddr),
				zap.ByteString("origin", helper.TruncateBytes([]byte(r.Header.Get("Origin")), fflogger.MAX_FIELD_LENGTH)),
			)
			http.Error(w, "Browser origins are not allowed", http.StatusForbidden)
			return
		}

		// Control-plane contact signal (see hub.contactObserver for the route
		// and loopback exclusions and why they are load-bearing).
		if h.contactObserver != nil && countsAsContact(route) && !isLoopbackAddr(r.RemoteAddr) {
			h.contactObserver()
		}

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

// playerLogsRoute is the one route a browser legitimately calls: the
// device's own player posting console logs (see handlePlayerLogs).
const playerLogsRoute = "player_logs"

// isBrowserOriginated reports whether a request carries the headers browsers
// attach to cross-site and page-initiated requests.
func isBrowserOriginated(r *http.Request) bool {
	return r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != ""
}

// localHostSuffixes are names only a local resolver answers: mDNS and the
// suffixes home routers hand out. None is registrable in public DNS, so a
// rebinding page cannot put one in Host.
var localHostSuffixes = []string{".local", ".lan", ".home", ".home.arpa", ".internal"}

// isLocalHost reports whether a request's Host names this device the way a
// native client on the LAN does: an IP literal, localhost, a single-label
// name, or a name under a local-only suffix. Anything else is a public DNS
// name, which is what a DNS-rebinding page carries.
func isLocalHost(hostport string) bool {
	host := hostport
	if h, port, err := net.SplitHostPort(hostport); err == nil {
		if _, perr := strconv.ParseUint(port, 10, 16); perr != nil {
			return false
		}
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return false
	}
	// A link-local IPv6 literal may carry a zone ("fe80::1%25wlan0").
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	if net.ParseIP(host) != nil {
		return true
	}
	if host == "localhost" || !strings.Contains(host, ".") {
		return true
	}
	for _, suffix := range localHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}
