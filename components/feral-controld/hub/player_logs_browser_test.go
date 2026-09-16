package hub

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandlePlayerLogsBrowserContract exercises the real handler across the
// shipping browser origins. It pairs the player's two delivery shapes with
// controld's actual CORS, JSON validation, attribution, and upstream response.
func TestHandlePlayerLogsBrowserContract(t *testing.T) {
	chrome := findChrome()
	if chrome == "" {
		if os.Getenv("FFOS_REQUIRE_LOG_BROWSER") == "1" {
			t.Fatal("Chrome or Chromium is required for the player log browser contract")
		}
		t.Skip("Chrome or Chromium not found")
	}

	forwarded := make(chan []playerLogRecord, 2)
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var records []playerLogRecord
		require.NoError(t, json.NewDecoder(r.Body).Decode(&records))
		forwarded <- records
		w.WriteHeader(http.StatusNoContent)
	})
	upstreamServer := &http.Server{Handler: upstream, ReadHeaderTimeout: time.Second}
	upstreamListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = upstreamServer.Serve(upstreamListener) }()

	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: "FF1-BROWSER"}},
		logEndpoint:    "http://" + upstreamListener.Addr().String(),
		logHTTPClient:  &http.Client{Timeout: 5 * time.Second},
	}
	var preflights atomic.Int32
	proxyListener, err := net.Listen("tcp", "127.0.0.1:1111")
	require.NoError(t, err)
	proxyServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			preflights.Add(1)
		}
		h.handlePlayerLogs(w, r)
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = proxyServer.Serve(proxyListener) }()

	page := `<!doctype html><script>
const record = message => ({timestamp:'2026-09-16T00:00:00.000Z',level:'info',environment:'test',message,context:{session_id:'browser-contract'}});
addEventListener('pagehide', () => fetch('http://127.0.0.1:1111/api/logs', {
  method:'POST', body:JSON.stringify([record('[AppContext] pagehide-keepalive')]), keepalive:true
}));
fetch('http://127.0.0.1:1111/api/logs', {
  method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify([record('[AppContext] idle-flushed-json')])
}).then(response => {
  if (!response.ok) throw new Error('proxy rejected idle batch');
  location.href='/done';
});
</script>`
	originListener, err := net.Listen("tcp", "127.0.0.1:8080")
	require.NoError(t, err)
	originServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/done" {
			_, _ = io.WriteString(w, "<!doctype html>done")
			return
		}
		_, _ = io.WriteString(w, page)
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = originServer.Serve(originListener) }()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	profile := t.TempDir()
	//nolint:gosec // The test only launches a discovered Chrome executable with fixed arguments.
	cmd := exec.CommandContext(ctx, chrome,
		"--headless=new", "--disable-gpu", "--no-sandbox",
		"--disable-dev-shm-usage", "--disable-background-networking",
		"--user-data-dir="+profile, playerOrigin+"/")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		cancel()
		_ = originServer.Shutdown(context.Background())
		_ = proxyServer.Shutdown(context.Background())
		_ = upstreamServer.Shutdown(context.Background())
		_ = cmd.Wait()
	})

	batches := make([][]playerLogRecord, 0, 2)
	for len(batches) < 2 {
		select {
		case batch := <-forwarded:
			batches = append(batches, batch)
		case <-ctx.Done():
			t.Fatalf("browser log contract timed out after %d batches", len(batches))
		}
	}

	assert.Equal(t, int32(1), preflights.Load())
	messages := map[string]bool{}
	for _, batch := range batches {
		require.Len(t, batch, 1)
		assert.Equal(t, "player", batch[0].Service)
		assert.Equal(t, "FF1-BROWSER", batch[0].DeviceID)
		assert.Equal(t, "browser-contract", batch[0].Context["session_id"])
		messages[batch[0].Message] = true
	}
	assert.True(t, messages["[AppContext] idle-flushed-json"])
	assert.True(t, messages["[AppContext] pagehide-keepalive"])
}

func findChrome() string {
	candidates := []string{
		os.Getenv("FFOS_LOG_BROWSER_BIN"),
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/usr/bin/chromium",
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		info, err := os.Stat(filepath.Clean(candidate))
		if err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}
