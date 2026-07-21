package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"net"
	go_http "net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// blobsRoutePrefix is the fixed path prefix the static server serves blobs
// under; kept as a constant so URLFor and the handler cannot drift apart.
const (
	blobsRoutePrefix = "/blobs/"
	// staticServerReadHeaderTimeout bounds how long a client may take to
	// send request headers. Loopback-only, but gosec G112 still applies;
	// matches hub's posture for local HTTP servers in this daemon.
	staticServerReadHeaderTimeout = 10 * time.Second
)

// StaticServer is the loopback HTTP fallback for assets over the ~400MB
// CDP Fetch.fulfillRequest body ceiling: base64-encoding a body that large
// blows the V8 string length limit. replay.go 302-redirects the kiosk
// browser here for such assets instead of fulfilling them inline from the
// blob store, so this server must stream from disk (via wrapper.OS.Open,
// not Store.ReadBlob, which reads a whole blob into memory) and honor
// Range requests so large video can still be seeked.
//
//go:generate mockgen -source=staticserver.go -destination=../mocks/offlinecache_staticserver.go -package=mocks -mock_names=StaticServer=MockOfflineCacheStaticServer
type StaticServer interface {
	// URLFor returns the loopback URL replay should redirect the kiosk to
	// for sha256Hex. contentType (if known) is embedded as a query
	// parameter and echoed back as the response's Content-Type, since the
	// server itself has no item metadata to look it up from. headers
	// (the captured Resource.Headers subset — see
	// filterReplayableHeaders' doc) is embedded the same way and echoed
	// back verbatim by handleBlob, so a large cross-origin asset served
	// through this loopback fallback still carries the CORS headers
	// Chromium's own enforcement checks against the FINAL response of a
	// redirect chain.
	URLFor(sha256Hex, contentType string, headers map[string]string) string
	// BaseURL returns "http://<addr>" so replay.go can recognize requests
	// aimed at this server (the follow-up request Chromium makes after a
	// 302 replay itself issued) and let them pass through Fetch
	// interception untouched instead of looping back into replay logic.
	BaseURL() string
	// Handler exposes the underlying mux for tests (httptest.NewServer) and
	// for embedding into another server if ever needed; production code
	// uses Listen+Serve.
	Handler() go_http.Handler
	// Listen synchronously binds addr and returns the bind error
	// immediately. This is deliberately split out of Serve (net/http's
	// ListenAndServe otherwise combines bind+serve into a single
	// blocking call): a caller that only ever launches ListenAndServe
	// in a background goroutine (as this server's own main.go wiring
	// used to) can only ever learn about a bind failure asynchronously,
	// via a log line, well after the rest of daemon startup has already
	// proceeded assuming success — including, critically, after
	// Replayer may have already started 302-redirecting large cached
	// assets to a server that either never bound at all (dead
	// redirects) or, worse, whose port was actually claimed by some
	// OTHER unrelated loopback process (redirects silently served by
	// the wrong service). Calling Listen first and gating both the
	// Serve goroutine AND IsListening on its result closes both cases.
	// Idempotent: calling it again after a successful bind is a no-op.
	Listen() error
	// IsListening reports whether Listen has already bound successfully.
	// Replayer's large-asset redirect path (see replay.go's
	// fulfillFromBlob) must check this before calling URLFor: redirecting
	// to a server that is not actually listening would otherwise produce
	// a dead redirect (nobody home) or, worse, one absorbed by an
	// unrelated service that happens to occupy the same loopback port.
	IsListening() bool
	// Serve blocks, accepting connections on the listener Listen bound.
	// Returns an error if Listen has not succeeded yet — callers must
	// call Listen first and check its error before spawning Serve in a
	// goroutine, rather than relying on Serve to bind for them. Once
	// Serve returns, for ANY reason (graceful Shutdown or an unexpected
	// accept/listener failure), IsListening reports false: replay's
	// large-asset redirect gate must not keep treating this server as
	// available once it has genuinely stopped accepting connections.
	Serve() error
	// ListenAndServe is a convenience one-call form (Listen then Serve)
	// kept for callers, such as most existing tests, that do not need
	// to observe the bind result separately from the blocking serve
	// call. Production wiring (main.go) uses Listen/Serve directly so it
	// CAN observe that result.
	ListenAndServe() error
	Shutdown(ctx context.Context) error
}

type staticServer struct {
	addr   string // host:port this server listens on and that URLFor builds URLs against
	store  Store
	os     wrapper.OS
	server wrapper.HTTPServer
	logger *zap.Logger

	mu       sync.RWMutex
	listener net.Listener // set once by a successful Listen; Shutdown never clears this (see Shutdown's doc)
	closed   bool         // flipped true by Shutdown; IsListening consults this instead of listener==nil
}

// NewStaticServer builds a StaticServer bound to addr (expected a loopback
// address such as 127.0.0.1:8082 — see the offlineCache.staticServerAddr
// config). addr is passed through safeLoopbackAddr first: this server
// exists purely as a kiosk-loopback fallback for oversized cached-artwork
// blobs and has no auth of its own, so a config typo must never be able to
// expose it over the LAN. It does not start listening; call Listen then
// Serve (or ListenAndServe for the combined, non-observable form).
func NewStaticServer(addr string, store Store, osWrapper wrapper.OS, logger *zap.Logger) StaticServer {
	addr = safeLoopbackAddr(addr, logger)
	s := &staticServer{addr: addr, store: store, os: osWrapper, logger: logger}
	mux := go_http.NewServeMux()
	mux.HandleFunc(blobsRoutePrefix, s.handleBlob)
	s.server = wrapper.NewHTTPServer(&go_http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: staticServerReadHeaderTimeout,
	})
	return s
}

// safeLoopbackAddr defends against a misconfigured offlineCache.
// staticServerAddr accidentally exposing cached artwork blobs over the LAN
// with no authentication of any kind. This server exists purely as a
// kiosk-loopback fallback for assets over the CDP body ceiling (see the
// package doc), so any host that is not a literal loopback address (or the
// "localhost" alias) is coerced to 127.0.0.1 with the configured port
// preserved — including the unspecified host net.Listen would otherwise
// treat as "all interfaces" (e.g. ":8082"), which is the most dangerous
// case because it looks harmless in config. A parse failure (missing
// port, garbage input) falls back to DefaultStaticServerAddr entirely
// rather than trying to guess a port to preserve. Every case logs at
// Error so a config typo is visible in the daemon's own logs rather than
// silently binding somewhere reachable from the network.
func safeLoopbackAddr(addr string, logger *zap.Logger) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		logger.Error("offline cache static server: staticServerAddr is not a valid host:port, forcing loopback default",
			zap.String("configured", addr), zap.String("fallback", DefaultStaticServerAddr), zap.Error(err))
		return DefaultStaticServerAddr
	}
	if isLoopbackHost(host) {
		return addr
	}
	safe := net.JoinHostPort("127.0.0.1", port)
	logger.Error("offline cache static server: staticServerAddr host is not loopback, forcing 127.0.0.1 to avoid exposing cached artwork blobs over the network",
		zap.String("configured", addr), zap.String("forced", safe))
	return safe
}

// isLoopbackHost reports whether host (as split from a host:port pair,
// so never containing brackets) is safe for this loopback-only server.
func isLoopbackHost(host string) bool {
	if host == "" {
		// net.Listen treats an empty host as "listen on all interfaces"
		// — exactly the unsafe case this function exists to reject —
		// so this must not be treated as loopback despite
		// net.ParseIP("") also failing below anyway.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *staticServer) URLFor(sha256Hex, contentType string, headers map[string]string) string {
	u := url.URL{
		Scheme: "http",
		Host:   s.addr,
		Path:   blobsRoutePrefix + sha256Hex,
	}
	q := url.Values{}
	if contentType != "" {
		q.Set("ct", contentType)
	}
	// Each header is its own repeated "h=Name=Value" query entry (rather
	// than one JSON-encoded blob) so the URL stays easy to eyeball in
	// logs; url.Values.Encode() handles percent-encoding the value half,
	// and handleBlob only ever splits on the FIRST "=" when decoding, so
	// a "=" inside the value itself round-trips correctly.
	for name, value := range headers {
		q.Add("h", name+"="+value)
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String()
}

func (s *staticServer) handleBlob(w go_http.ResponseWriter, r *go_http.Request) {
	if r.Method != go_http.MethodGet && r.Method != go_http.MethodHead {
		go_http.Error(w, "method not allowed", go_http.StatusMethodNotAllowed)
		return
	}

	sha256Hex := strings.TrimPrefix(r.URL.Path, blobsRoutePrefix)
	if sha256Hex == "" || strings.ContainsAny(sha256Hex, "/\\") {
		go_http.Error(w, "invalid blob id", go_http.StatusBadRequest)
		return
	}

	if _, err := s.store.BlobSize(sha256Hex); err != nil {
		s.logger.Debug("offline cache static server: blob not found",
			zap.String("sha256", sha256Hex), zap.Error(err))
		go_http.NotFound(w, r)
		return
	}

	f, err := s.os.Open(s.store.BlobPath(sha256Hex))
	if err != nil {
		s.logger.Warn("offline cache static server: failed to open blob",
			zap.String("sha256", sha256Hex), zap.Error(err))
		go_http.Error(w, "failed to open asset", go_http.StatusInternalServerError)
		return
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			s.logger.Warn("offline cache static server: failed to close blob file", zap.Error(closeErr))
		}
	}()

	if ct := r.URL.Query().Get("ct"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Re-validate against the same allowlist URLFor's caller (replay.go)
	// already filtered through — defense in depth against this handler
	// ever echoing an unexpected header if URLFor's contract is bypassed
	// or a URL is somehow crafted by hand, since this is a bare loopback
	// endpoint with no other authentication of its own.
	for _, pair := range r.URL.Query()["h"] {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || !isReplayableHeaderName(name) {
			continue
		}
		w.Header().Set(name, value)
	}

	info, err := f.Stat()
	if err != nil {
		go_http.Error(w, "failed to stat asset", go_http.StatusInternalServerError)
		return
	}

	// http.ServeContent honors Range requests via the io.ReadSeeker (the
	// *os.File), which is required for video seeking on assets this large.
	go_http.ServeContent(w, r, sha256Hex, info.ModTime(), f)
}

func (s *staticServer) BaseURL() string {
	return "http://" + s.addr
}

func (s *staticServer) Handler() go_http.Handler {
	return s.server.Handler()
}

func (s *staticServer) Listen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil && !s.closed {
		return nil
	}
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("offline cache static server: listen on %s: %w", s.addr, err)
	}
	s.listener = l
	s.closed = false
	return nil
}

func (s *staticServer) IsListening() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listener != nil && !s.closed
}

func (s *staticServer) Serve() error {
	// Reads listener, never closed — see Shutdown's doc for why Shutdown
	// deliberately never clears this field: doing so could race a Serve
	// call whose goroutine had not yet reached this read (Serve is
	// always launched in its own goroutine right after Listen succeeds
	// — see main.go's wiring), making Serve spuriously report "before
	// Listen succeeded" even though Listen genuinely already had.
	s.mu.RLock()
	l := s.listener
	s.mu.RUnlock()
	if l == nil {
		return errors.New("offline cache static server: Serve called before Listen succeeded")
	}
	// Serve blocks until serving genuinely stops, for ANY reason — a
	// graceful Shutdown (which already set closed=true itself, so this
	// is a harmless no-op re-assignment) OR an unexpected accept/listener
	// failure that Shutdown never saw coming. Without marking closed here
	// too, an unexpected Serve exit would leave IsListening() reporting
	// true forever after despite nothing actually being accepted anymore,
	// so replay.go's large-asset redirect gate would keep sending the
	// kiosk to a server that has stopped listening — reintroducing the
	// exact dead-redirect risk Listen/IsListening was split out to close.
	defer func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
	}()
	if err := s.server.Serve(l); err != nil && !errors.Is(err, go_http.ErrServerClosed) {
		return fmt.Errorf("offline cache static server: %w", err)
	}
	return nil
}

// ListenAndServe binds and serves in one blocking call for callers that
// do not need to observe the bind result separately (see the interface
// doc). It deliberately still calls Listen first rather than delegating
// straight to s.server.ListenAndServe(), so IsListening reflects reality
// for these callers too, not only for main.go's split Listen/Serve path.
func (s *staticServer) ListenAndServe() error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve()
}

// Shutdown deliberately never nils s.listener (only Serve reads it, and
// only ever to hand it to s.server.Serve — see Serve's doc for the
// race that would otherwise create): net/http's own Shutdown already
// makes any Serve call on this same *http.Server return
// ErrServerClosed immediately once shutdown has begun, regardless of
// whether this wrapper's own listener field is cleared or not, so
// clearing it here would only add risk without adding any actual
// safety. IsListening instead consults the separate closed flag.
func (s *staticServer) Shutdown(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return err
}
