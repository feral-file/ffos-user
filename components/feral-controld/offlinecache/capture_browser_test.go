package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// This file holds the ONE capture-guard test that uses a real Chromium
// instead of a scripted CDPSession. Every other guard test in this
// package drives a fake session, which pins what the capturer does with
// the events it is handed but can say nothing about whether Chromium
// actually hands it those events — whether Fetch interception is armed
// before the page can issue its first request, and whether it reaches
// requests made from a worker rather than the root document. Those are
// assumptions about another program's behavior, and the fake cannot
// falsify them; only a browser can.
//
// What this test does NOT establish, stated plainly so nobody reads more
// into a pass than is there: it does not cover a true cross-site OOPIF,
// which needs two real registrable domains and therefore real DNS, so
// that leg remains covered only by the fake-session tests. It also does
// not exercise the systemd scope, which is orthogonal to the guard.
//
// WHERE THIS ACTUALLY RUNS, because the answer is not "everywhere" and
// assuming otherwise makes the coverage look broader than it is: it needs
// a Chromium that both exists AND launches. GitHub's ubuntu-latest has a
// preinstalled Chrome that never comes up under the runner's AppArmor
// restriction on unprivileged user namespaces, and the shipped argv
// deliberately passes no --no-sandbox, so on CI this SKIPS. Today the
// only place it really executes is the FF1 (and any dev machine with a
// working Chromium). Do not read a green CI run as evidence this passed.
//
// Adding --no-sandbox to make CI run it would mean testing a spawn we do
// not ship, which is worse than skipping.
//
// Two skips, for two different environments: no binary at all, and a
// binary that cannot start (ErrHeadlessNotReady). The first version of
// this file had only the former and turned CI red. Set
// FFOS_CAPTURE_BROWSER_BIN to point at a specific binary.

func findChromiumForTest() string {
	if p := os.Getenv("FFOS_CAPTURE_BROWSER_BIN"); p != "" {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// freeTCPPortForTest reserves a port by binding and immediately closing.
// Racy in principle; acceptable here because the alternative (letting
// Chromium pick via --remote-debugging-port=0 and parsing DevToolsPort)
// is not reachable through the Downloader seam this test exists to
// exercise as-shipped.
func freeTCPPortForTest(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// TestCapturer_RealBrowser_LoopbackRequestsNeverLeaveTheBrowser is the
// end-to-end statement of the guard's purpose: a captured artwork cannot
// reach a loopback service, no matter which browsing context it asks
// from.
//
// The page is a data: URL because every alternative is itself blocked —
// an httptest server is on loopback and a LAN address is private, so
// there is no "permitted" origin to serve real HTML from without
// external DNS. data: is permitted by design (the bytes are already in
// hand; there is nothing to dial), which makes it the only usable
// stand-in for a permitted page.
//
// Two assertions, and the second is what keeps the first honest:
//
//  1. the victim listener recorded zero connections, and
//  2. the capturer logged a block naming the victim host.
//
// (1) alone would pass just as happily if Chromium never started, the
// page never loaded, or the script never ran — the exact failures most
// likely to make a browser test quietly vacuous. (2) proves the requests
// were really issued and really stopped.
func TestCapturer_RealBrowser_LoopbackRequestsNeverLeaveTheBrowser(t *testing.T) {
	binary := findChromiumForTest()
	if binary == "" {
		t.Skip("no chromium binary found; set FFOS_CAPTURE_BROWSER_BIN to run this test")
	}

	// The victim: a loopback listener standing in for any local service
	// (controld's own API, the DevTools endpoint, a metrics port). Counts
	// at the ACCEPT layer, not the HTTP handler, so even a connection
	// whose request never completes is still recorded as a breach.
	var hits int64
	victim, lerr := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, lerr)
	defer func() { _ = victim.Close() }()
	victimAddr := victim.Addr().String()
	victimHost, _, herr := net.SplitHostPort(victimAddr)
	require.NoError(t, herr)
	go func() {
		for {
			conn, aerr := victim.Accept()
			if aerr != nil {
				return
			}
			atomic.AddInt64(&hits, 1)
			_ = conn.Close()
		}
	}()
	victimURL := "http://" + victimAddr + "/breach"

	// The artwork needs a PERMITTED origin to be served from, and every
	// address available to a test is reserved. So: serve it from
	// 127.0.0.2 and override the guard's address policy for that ONE
	// address. The override delegates everything else to the real
	// isReservedAddr, so the victim on 127.0.0.1 is still judged by the
	// production predicate — what is faked is where the artwork lives,
	// never whether the victim is off limits.
	//
	// This is why the page is real HTML over HTTP rather than a data:
	// URL: a data: document has an opaque origin, which cannot construct
	// a blob Worker at all, so the worker leg — the auto-attach path,
	// the whole reason a real browser is needed here — silently never
	// runs. An earlier version of this test did exactly that and passed
	// on three blocks while proving nothing about workers.
	artworkIP := net.ParseIP("127.0.0.2")
	artworkLn, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot bind 127.0.0.2 (needed for a permitted origin): %v", err)
	}
	defer func() { _ = artworkLn.Close() }()

	// Four contexts, because the guard has to hold in all of them: the
	// root document (fetch and a subresource load), an iframe, and a
	// dedicated worker. The worker is contained by a DIFFERENT mechanism
	// than the other three — see the worker assertion at the bottom.
	html := strings.NewReplacer("__URL__", victimURL).Replace(
		`<html><body><script>
fetch('__URL__/root-fetch').catch(function(){});
var i = new Image(); i.src = '__URL__/root-img';
var f = document.createElement('iframe');
f.src = '/iframe.html';
document.body.appendChild(f);
var b = new Blob(["fetch('__URL__/worker-fetch').catch(function(){});"], {type:'text/javascript'});
new Worker(URL.createObjectURL(b));
</script></body></html>`)
	iframeHTML := strings.NewReplacer("__URL__", victimURL).Replace(
		`<html><body><script>fetch('__URL__/iframe-fetch').catch(function(){});</script></body></html>`)

	artworkSrv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/iframe.html" {
			_, _ = w.Write([]byte(iframeHTML))
			return
		}
		_, _ = w.Write([]byte(html))
	})}
	go func() { _ = artworkSrv.Serve(artworkLn) }()
	defer func() { _ = artworkSrv.Close() }()
	page := "http://" + artworkLn.Addr().String() + "/index.html"

	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	// Deliberately NOT t.TempDir: its cleanup is a hard test failure, and
	// Chromium's profile directory keeps being written for a moment after
	// Close() returns, so t.TempDir turns an orderly shutdown race into a
	// red test that says nothing about the guard. Observed on the FF1.
	tmp, err := os.MkdirTemp("", "capture-guard-browser-")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(tmp) }()
	downloader := NewDownloader(
		binary,
		filepath.Join(tmp, "chrome-profile"),
		freeTCPPortForTest(t),
		30*time.Second,
		HeadlessLimits{}, // scope/pin disabled: orthogonal to the guard
		wrapper.NewExec(),
		wrapper.NewOS(),
		wrapper.NewClock(),
		wrapper.NewHTTPClient(),
		logger,
	)

	cap := NewCapturer(
		downloader,
		wrapper.NewWebSocketDialer(websocket.DefaultDialer),
		wrapper.NewHTTPClient(),                      // CDP discovery: loopback by design, must stay unguarded
		newGuardedHTTPClient(net.DefaultResolver, 0), // resource bodies: guarded
		net.DefaultResolver,
		NewStore(filepath.Join(tmp, "cache"), wrapper.NewOS(), wrapper.NewJSON(), logger),
		wrapper.NewJSON(),
		wrapper.NewIO(),
		wrapper.NewClock(),
		1<<20,
		logger,
	)
	// Permit ONLY the artwork's address; everything else, including the
	// victim, still goes through the shipped predicate.
	cap.(*capturer).guard.isReserved = func(ip net.IP) bool {
		if ip.Equal(artworkIP) {
			return false
		}
		return isReservedAddr(ip)
	}
	defer func() { _ = cap.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A capture error is not a failure condition here. This page has no
	// capturable resources by construction, and the question under test
	// is what escaped the browser, not what got stored.
	//
	// ErrHeadlessNotReady is the one exception, and it must SKIP rather
	// than fail: it means no usable browser in this environment (GitHub's
	// runners have a chromium binary that never comes up — likely the
	// sandbox), so every assertion below would be measuring nothing. That
	// is precisely the vacuum the log assertion exists to catch, so
	// letting it fail there would be the check working correctly on a
	// question nobody asked. Note the binary-presence skip at the top
	// cannot cover this: the binary IS present, it just cannot run.
	_, cerr := cap.Capture(ctx, dp1playlist.PlaylistItem{ID: "browser-guard", Source: page}, 8000)
	if errors.Is(cerr, ErrHeadlessNotReady) {
		t.Skipf("chromium present but never became ready (%v); no browser to test against here", cerr)
	}
	if cerr != nil {
		t.Logf("capture returned %v (not a failure for this test)", cerr)
	}

	require.Equal(t, int64(0), atomic.LoadInt64(&hits),
		"the capture browser reached the loopback victim %s: the guard did not hold", victimAddr)

	var blocked []string
	for _, e := range logs.All() {
		if !strings.Contains(e.Message, "blocked page request") {
			continue
		}
		blocked = append(blocked, e.Message+" "+fmt.Sprint(e.ContextMap()))
	}
	t.Logf("guard blocked %d request(s):\n%s", len(blocked), strings.Join(blocked, "\n"))
	require.NotEmpty(t, blocked,
		"no blocked-request log: the page never reached the guard, so the zero-hit result above proves nothing")

	all := strings.Join(blocked, "\n")
	for _, leg := range []string{"root-fetch", "root-img", "iframe-fetch"} {
		require.Contains(t, all, leg,
			"the %s request never reached the guard; this leg of the test is vacuous", leg)
	}
	require.Contains(t, all, victimHost, "blocks were logged but none named the victim host")

	// The worker leg, and the reason it gets its own assertion: Chromium
	// does not implement the Fetch domain on worker targets. Measured on
	// the FF1 — Fetch.enable on a type:worker session returns
	// "cdp error -32601: 'Fetch.enable' wasn't found". So a worker is NOT
	// contained by interception the way the three legs above are; it is
	// contained by staying paused, because armCaptureChildTarget fails
	// closed and never sends Runtime.runIfWaitingForDebugger when it
	// cannot arm interception first.
	//
	// Either outcome is safe, and which one applies is Chromium's choice,
	// not ours — so this accepts both and fails only if the worker
	// ESCAPED both. That is the regression worth pinning: it catches a
	// future edit that "fixes" the -32601 path by resuming the target
	// anyway, which would silently hand every artwork worker an
	// unguarded network.
	var childPaused bool
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "leaving it paused") && fmt.Sprint(e.ContextMap()["type"]) == "worker" {
			childPaused = true
		}
	}
	require.True(t, childPaused || strings.Contains(all, "worker-fetch"),
		"the worker was neither intercepted nor left paused, yet issued no request: "+
			"containment cannot be confirmed, so this leg proves nothing")
}
