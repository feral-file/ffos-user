package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

var (
	// Flags
	listen  = flag.String("listen", "0.0.0.0:3000", "listen address")
	docroot = flag.String("docroot", "player/out", "static site root (Next.js export)")

	// HTTP client with sane timeouts and redirect following
	client = &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 8 * time.Second,
		},
		Timeout: 60 * time.Second,
	}

	// listenAndServe allows tests to stub out the blocking server call.
	listenAndServe = func(srv *http.Server) error {
		return srv.ListenAndServe()
	}
)

func main() {
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", health)

	// Root: either proxy when ?url= is present, or serve static site
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("url") != "" {
			proxy(w, r)
			return
		}
		// Serve static
		fs := http.FileServer(neuteredFS{http.Dir(*docroot)})
		fs.ServeHTTP(w, r)
	})
	mux.Handle("/", root)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           withCORS(withTimeout(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("feral-player-ui listening on http://%s (docroot: %s)", *listen, *docroot)
	if err := listenAndServe(srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// Adds CORS and Private Network Access headers for calls to "/" when proxying (?url=...)
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preflight only when using proxy mode (OPTIONS + has ?url=)
		if r.Method == http.MethodOptions && r.URL.Path == "/" && r.URL.Query().Get("url") != "" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "*")
			w.Header().Set("Access-Control-Allow-Private-Network", "true") // PNA
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func proxy(w http.ResponseWriter, r *http.Request) {
	// Only GET/HEAD to keep media behavior predictable
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodOptions {
		// Preflight handled in withCORS; duplicate safety here
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	raw := r.URL.Query().Get("url")
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Scheme == "" || u.Host == "" {
		http.Error(w, "bad or missing url", http.StatusBadRequest)
		return
	}

	// Build upstream request, pass Range (important for HLS/MP4)
	upReq, _ := http.NewRequestWithContext(r.Context(), r.Method, u.String(), nil)
	if rng := r.Header.Get("Range"); rng != "" {
		upReq.Header.Set("Range", rng)
	}
	// If needed later: upReq.Header.Set("Authorization", r.Header.Get("Authorization"))

	upRes, err := client.Do(upReq)
	if err != nil {
		http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upRes.Body.Close()

	// Mirror key headers for media playback
	for _, h := range []string{
		"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range",
		"Cache-Control", "Expires", "Last-Modified", "ETag",
	} {
		if v := upRes.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}

	// CORS + PNA on actual response too
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Private-Network", "true")
	w.Header().Set("Vary", "Origin")

	w.WriteHeader(upRes.StatusCode)
	if r.Method == http.MethodHead {
		return
	}
	io.Copy(w, upRes.Body)
}

// neuteredFS prevents directory listings and ensures /path/ resolves to /path/index.html
type neuteredFS struct{ fs http.FileSystem }

func (nfs neuteredFS) Open(name string) (http.File, error) {
	f, err := nfs.fs.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		index := filepath.Join(name, "index.html")
		if _, err := nfs.fs.Open(index); err != nil {
			return nil, os.ErrNotExist
		}
	}
	return f, nil
}
