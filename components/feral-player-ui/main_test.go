package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()

	health(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rr.Code)
	}
	if got := rr.Body.String(); got != `{"ok":true}` {
		t.Fatalf("unexpected body %q", got)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json content-type, got %q", ct)
	}
}

func TestWithCORSPreflight(t *testing.T) {
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/?url=http://example.com", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected %d, got %d", http.StatusNoContent, rr.Code)
	}

	tests := map[string]string{
		"Access-Control-Allow-Origin":          "*",
		"Access-Control-Allow-Methods":         "GET, HEAD, OPTIONS",
		"Access-Control-Allow-Headers":         "*",
		"Access-Control-Allow-Private-Network": "true",
	}
	for key, want := range tests {
		if got := rr.Header().Get(key); got != want {
			t.Fatalf("header %q: want %q, got %q", key, want, got)
		}
	}
}

func TestWithCORSPassThrough(t *testing.T) {
	var called bool
	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatalf("expected wrapped handler to be invoked")
	}
	if rr.Code != http.StatusTeapot {
		t.Fatalf("expected %d, got %d", http.StatusTeapot, rr.Code)
	}
}

func TestProxyRejectsBadURL(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/?url=://", nil)
	rr := httptest.NewRecorder()

	proxy(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected %d, got %d", http.StatusBadRequest, rr.Code)
	}
}

func TestProxyRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/?url=http://example.com", nil)
	rr := httptest.NewRecorder()

	proxy(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected %d, got %d", http.StatusMethodNotAllowed, rr.Code)
	}
}

func TestProxySuccessCopiesHeadersAndBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=0-100" {
			t.Fatalf("expected Range header to be forwarded, got %q", got)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Cache-Control", "public, max-age=60")
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "proxied")
	}))
	t.Cleanup(upstream.Close)

	prevClient := client
	client = upstream.Client()
	t.Cleanup(func() { client = prevClient })

	target := upstream.URL + "/media"
	req := httptest.NewRequest(http.MethodGet, "/?url="+url.QueryEscape(target), nil)
	req.Header.Set("Range", "bytes=0-100")
	rr := httptest.NewRecorder()

	proxy(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("expected %d, got %d", http.StatusPartialContent, rr.Code)
	}
	if body := rr.Body.String(); body != "proxied" {
		t.Fatalf("unexpected body %q", body)
	}

	headers := map[string]string{
		"Content-Type":                         "text/plain",
		"Cache-Control":                        "public, max-age=60",
		"Access-Control-Allow-Origin":          "*",
		"Access-Control-Allow-Private-Network": "true",
		"Vary":                                 "Origin",
	}
	for key, want := range headers {
		if got := rr.Header().Get(key); got != want {
			t.Fatalf("header %q: want %q, got %q", key, want, got)
		}
	}
}

func TestProxyHeadRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "123")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	prevClient := client
	client = upstream.Client()
	t.Cleanup(func() { client = prevClient })

	req := httptest.NewRequest(http.MethodHead, "/?url="+url.QueryEscape(upstream.URL), nil)
	rr := httptest.NewRecorder()

	proxy(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected %d, got %d", http.StatusOK, rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("expected empty body for HEAD request, got %q", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Length"); got != "123" {
		t.Fatalf("expected Content-Length header to be mirrored, got %q", got)
	}
}

func TestProxyHandlesOptionsRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/?url=http://example.com", nil)
	rr := httptest.NewRecorder()

	proxy(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("expected %d, got %d", http.StatusNoContent, rr.Code)
	}
	if allowMethods := rr.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(allowMethods, "OPTIONS") {
		t.Fatalf("expected Access-Control headers to be set for OPTIONS request, got %q", allowMethods)
	}
}

type errorRoundTripper struct{ err error }

func (e errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

func TestProxyUpstreamError(t *testing.T) {
	prevClient := client
	client = &http.Client{Transport: errorRoundTripper{err: errors.New("boom")}, Timeout: time.Second}
	t.Cleanup(func() { client = prevClient })

	req := httptest.NewRequest(http.MethodGet, "/?url=http://example.com", nil)
	rr := httptest.NewRecorder()

	proxy(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected %d, got %d", http.StatusBadGateway, rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "proxy error") {
		t.Fatalf("expected proxy error message, got %q", body)
	}
}

func TestWithTimeoutSetsDeadline(t *testing.T) {
	var (
		ctx    context.Context
		called bool
	)

	handler := withTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		ctx = r.Context()
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	before := time.Now()
	handler.ServeHTTP(rr, req)

	if !called {
		t.Fatalf("expected wrapped handler to be invoked")
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected context to have deadline")
	}
	if ttl := deadline.Sub(before); ttl < 70*time.Second || ttl > 80*time.Second {
		t.Fatalf("expected deadline ~75s out, got %s", ttl)
	}

	select {
	case <-ctx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("expected context to be canceled after handler returns")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context to be canceled, got %v", ctx.Err())
	}
}

func TestNeuteredFSRejectsDirectoryWithoutIndex(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "static", "images"), 0o755); err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	fs := neuteredFS{http.Dir(root)}
	_, err := fs.Open("/static/images")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist, got %v", err)
	}
}

func TestNeuteredFSServesDirectoryWithIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	indexPath := filepath.Join(dir, "index.html")
	if err := os.WriteFile(indexPath, []byte("ok"), 0o644); err != nil {
		t.Fatalf("failed to write index: %v", err)
	}

	fs := neuteredFS{http.Dir(root)}
	dirHandle, err := fs.Open("/app")
	if err != nil {
		t.Fatalf("expected directory with index to open, got %v", err)
	}
	t.Cleanup(func() { dirHandle.Close() })

	file, err := fs.Open("/app/index.html")
	if err != nil {
		t.Fatalf("expected index file to open, got %v", err)
	}
	t.Cleanup(func() { file.Close() })

	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}
	if string(data) != "ok" {
		t.Fatalf("unexpected file contents %q", data)
	}
}

func TestMainInitializesServer(t *testing.T) {
	origListen := listen
	origDocroot := docroot
	origCommandLine := flag.CommandLine
	origListenAndServe := listenAndServe
	t.Cleanup(func() {
		listen = origListen
		docroot = origDocroot
		flag.CommandLine = origCommandLine
		listenAndServe = origListenAndServe
	})

	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	listen = flag.String("listen", "127.0.0.1:0", "listen address")
	docroot = flag.String("docroot", t.TempDir(), "static site root (Next.js export)")

	var called bool
	listenAndServe = func(srv *http.Server) error {
		called = true
		if srv.Addr != *listen {
			t.Fatalf("unexpected listen addr %q", srv.Addr)
		}

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		srv.Handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("expected health endpoint to be wired, got %d", rr.Code)
		}
		return http.ErrServerClosed
	}

	main()

	if !called {
		t.Fatalf("expected listenAndServe to be invoked")
	}
}
