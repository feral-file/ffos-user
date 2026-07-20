package offlinecache

import (
	"context"
	"errors"
	"fmt"
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
// config). It does not start listening; call ListenAndServe.
func NewStaticServer(addr string, store Store, osWrapper wrapper.OS, logger *zap.Logger) StaticServer {
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
