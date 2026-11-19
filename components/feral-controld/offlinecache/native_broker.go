package offlinecache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// NativeBroker is a read-only, same-UID cache interface for the native host.
// Body I/O is streamed, isolated from the player command socket and bounded to
// four concurrent operations. Store mutations remain exclusively in controld.
type NativeBroker struct {
	store  Store
	slots  chan struct{}
	Policy func(string, map[string]string) (string, []HeaderField)
}
type brokerRequest struct {
	Source  string            `json:"source"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
}
type brokerMetadata struct {
	Version        int           `json:"version"`
	Status         int           `json:"status"`
	Headers        []HeaderField `json:"headers"`
	Size           int64         `json:"size"`
	SHA256         string        `json:"sha256"`
	Complete       bool          `json:"complete"`
	Path           string        `json:"path,omitempty"`
	ReplayMode     string        `json:"replayMode,omitempty"`
	NativeMIME     string        `json:"nativeMime,omitempty"`
	RequestURL     string        `json:"requestUrl,omitempty"`
	RequestHeaders []HeaderField `json:"requestHeaders,omitempty"`
}

func NewNativeBroker(store Store) *NativeBroker {
	return &NativeBroker{store: store, slots: make(chan struct{}, 4)}
}
func (b *NativeBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || (r.URL.Path != "/v1/replay" && r.URL.Path != "/v1/open" && r.URL.Path != "/v1/policy") {
		http.NotFound(w, r)
		return
	}
	var req brokerRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || req.Source == "" || len(req.Source) > 16384 || len(req.URL) > 16384 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	var trailing any
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		http.Error(w, "trailing data", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/v1/policy" {
		target := req.URL
		fields := []HeaderField{}
		if b.Policy != nil {
			target, fields = b.Policy(req.URL, req.Headers)
		}
		metadata := brokerMetadata{Version: 1, Status: 200, Headers: []HeaderField{}, RequestURL: target, RequestHeaders: fields}
		if req.Source == req.URL && b.store != nil {
			if rec, err := b.store.LoadItem(SourceKey(req.Source)); err == nil && nativeReplayRecordReady(rec) {
				metadata.ReplayMode = "isolated"
				if rec.Automatic {
					metadata.ReplayMode = "mixed"
				}
			}
		}
		encoded, err := json.Marshal(metadata)
		if err != nil || len(encoded) > 128<<10 {
			http.Error(w, "policy limit", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.feralfile.cache-resource-v1")
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(encoded))) // #nosec G115 -- metadata bounded above
		if _, err = w.Write(prefix[:]); err == nil {
			_, _ = w.Write(encoded)
		}
		return
	}
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	if b.store == nil {
		http.Error(w, "cache disabled", http.StatusNotFound)
		return
	}
	if req.Method != "GET" && req.Method != "HEAD" {
		http.Error(w, "unsupported method", http.StatusUnprocessableEntity)
		return
	}
	for name, value := range req.Headers {
		if value != "" && (strings.EqualFold(name, "Cookie") || strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Range")) {
			http.Error(w, "unsupported request variant", http.StatusUnprocessableEntity)
			return
		}
	}
	rec, err := b.store.LoadItem(SourceKey(req.Source))
	if err != nil {
		http.Error(w, "cache unavailable", http.StatusNotFound)
		return
	}
	var found *Resource
	for i := range rec.Resources {
		res := &rec.Resources[i]
		if res.URL == req.URL && res.EffectiveMethod() == "GET" {
			if found != nil {
				http.Error(w, "ambiguous capture", http.StatusConflict)
				return
			}
			found = res
		}
	}
	if found == nil || !found.freshAt(time.Now()) {
		http.Error(w, "resource unavailable", http.StatusNotFound)
		return
	}
	res := *found
	// Legacy files can be routed to the native decoder without fabricating
	// browser headers. This bodyless classification is valid only for the entry
	// URL; the browser adapter never fulfills an artist resource with it.
	mime := strings.ToLower(res.ProbeContentType())
	native := (strings.HasPrefix(mime, "image/") && !strings.HasPrefix(mime, "image/svg")) || strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/")
	if r.URL.Path == "/v1/replay" && req.URL == req.Source && native {
		metadata := brokerMetadata{Version: 1, Status: 200, Headers: []HeaderField{}, NativeMIME: mime}
		encoded, _ := json.Marshal(metadata)
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(encoded))) // #nosec G115 -- fixed-size bounded metadata
		_, _ = w.Write(prefix[:])
		_, _ = w.Write(encoded)
		return
	}
	if res.BrowserPolicyVersion != 2 && r.URL.Path != "/v1/open" {
		http.Error(w, "capture requires browser-policy recapture", http.StatusUnprocessableEntity)
		return
	}
	metadata := brokerMetadata{Version: 1, Status: res.Status, Headers: res.HeaderFields, Complete: rec.Coverage.Complete}
	if metadata.Status == 0 {
		metadata.Status = 200
	}
	var body *os.File
	if !res.IsRedirect() {
		digest, decodeErr := hex.DecodeString(res.SHA256)
		if decodeErr != nil || len(digest) != 32 {
			http.Error(w, "invalid digest", http.StatusInternalServerError)
			return
		}
		body, err = os.Open(b.store.BlobPath(res.SHA256))
		if err != nil {
			http.Error(w, "blob unavailable", http.StatusNotFound)
			return
		}
		defer func() { _ = body.Close() }()
		stat, statErr := body.Stat()
		if statErr != nil || !stat.Mode().IsRegular() {
			http.Error(w, "invalid blob", http.StatusInternalServerError)
			return
		}
		// Verify this opened inode before writing a success header. A concurrent GC
		// unlink cannot redirect the reader, and a torn blob never becomes success.
		hash := sha256.New()
		if _, err = io.Copy(hash, &brokerContextReader{ctx: r.Context(), reader: body}); err != nil || hex.EncodeToString(hash.Sum(nil)) != res.SHA256 {
			http.Error(w, "blob corrupt", http.StatusInternalServerError)
			return
		}
		if _, err = body.Seek(0, io.SeekStart); err != nil {
			http.Error(w, "blob seek", http.StatusInternalServerError)
			return
		}
		metadata.Size = stat.Size()
		metadata.SHA256 = res.SHA256
	}
	if req.Method == "HEAD" {
		metadata.Size = 0
	}
	if r.URL.Path == "/v1/open" {
		if body == nil {
			http.Error(w, "native file cannot be a redirect", http.StatusUnprocessableEntity)
			return
		}
		metadata.Path = b.store.BlobPath(res.SHA256)
	}
	if metadata.Headers == nil {
		metadata.Headers = []HeaderField{}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > 128<<10 {
		http.Error(w, "metadata limit", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.feralfile.cache-resource-v1")
	var prefix [4]byte
	// Metadata is bounded to 128 KiB above.
	binary.BigEndian.PutUint32(prefix[:], uint32(len(encoded))) // #nosec G115 -- bounded metadata length
	if _, err = w.Write(prefix[:]); err != nil {
		return
	}
	if _, err = w.Write(encoded); err != nil {
		return
	}
	if body != nil && req.Method != "HEAD" && r.URL.Path != "/v1/open" {
		_, _ = io.Copy(w, io.LimitReader(body, metadata.Size))
	}
}

func (b *NativeBroker) Run(ctx context.Context, path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("native cache socket directory is not private")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600) // #nosec G304 -- operator socket path, private same-UID directory, no-follow
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("native cache socket is already owned")
	}
	if info, err = os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("refusing non-socket cache path")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	defer func() { _ = os.Remove(path) }()
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	server := &http.Server{Handler: b, ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 16 << 10,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if !nativeCachePeer(c) {
				_ = c.Close()
			}
			return ctx
		}}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Hashing a large native file must stop when its requesting occurrence retires.
type brokerContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *brokerContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
