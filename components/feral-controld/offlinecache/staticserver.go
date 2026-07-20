package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"net"
	go_http "net/http"
	"net/url"
	"strings"
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
	// server itself has no item metadata to look it up from.
	URLFor(sha256Hex, contentType string) string
	// BaseURL returns "http://<addr>" so replay.go can recognize requests
	// aimed at this server (the follow-up request Chromium makes after a
	// 302 replay itself issued) and let them pass through Fetch
	// interception untouched instead of looping back into replay logic.
	BaseURL() string
	// Handler exposes the underlying mux for tests (httptest.NewServer) and
	// for embedding into another server if ever needed; production code
	// uses ListenAndServe.
	Handler() go_http.Handler
	ListenAndServe() error
	Shutdown(ctx context.Context) error
}

type staticServer struct {
	addr   string // host:port this server listens on and that URLFor builds URLs against
	store  Store
	os     wrapper.OS
	server wrapper.HTTPServer
	logger *zap.Logger
}

// NewStaticServer builds a StaticServer bound to addr (expected a loopback
// address such as 127.0.0.1:8082 — see the offlineCache.staticServerAddr
// config). addr is passed through safeLoopbackAddr first: this server
// exists purely as a kiosk-loopback fallback for oversized cached-artwork
// blobs and has no auth of its own, so a config typo must never be able to
// expose it over the LAN. It does not start listening; call
// ListenAndServe.
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

func (s *staticServer) URLFor(sha256Hex, contentType string) string {
	u := url.URL{
		Scheme: "http",
		Host:   s.addr,
		Path:   blobsRoutePrefix + sha256Hex,
	}
	if contentType != "" {
		q := url.Values{}
		q.Set("ct", contentType)
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

func (s *staticServer) ListenAndServe() error {
	if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, go_http.ErrServerClosed) {
		return fmt.Errorf("offline cache static server: %w", err)
	}
	return nil
}

func (s *staticServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
