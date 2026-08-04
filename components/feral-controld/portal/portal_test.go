package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer builds a portal Server with in-memory seams and an httptest
// server that does NOT follow redirects (so probe 302s are observable).
func newTestServer(t *testing.T, cfg Config) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	s := NewServer(cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return s, ts, client
}

func TestCaptiveProbesRedirectToPortal(t *testing.T) {
	_, ts, client := newTestServer(t, Config{APSSID: "FF1-abc"})

	probes := []string{
		"/generate_204",
		"/gen_204",
		"/hotspot-detect.html",
		"/library/test/success.html",
		"/connecttest.txt",
		"/ncsi.txt",
		"/anything-else", // unenumerated path: still bounced by the root handler
	}
	for _, p := range probes {
		t.Run(p, func(t *testing.T) {
			resp, err := client.Get(ts.URL + p)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			assert.Equal(t, http.StatusFound, resp.StatusCode)
			assert.Equal(t, "/", resp.Header.Get("Location"))
		})
	}
}

func TestRootRendersNetworksAndPrewarning(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-devicexyz",
		Scan: func(context.Context) ([]string, error) {
			return []string{"HomeNet", "Cafe-5G"}, nil
		},
	})
	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "HomeNet")
	assert.Contains(t, body, "Cafe-5G")
	// AP-bounce pre-warning names the AP SSID to reconnect to.
	assert.Contains(t, body, "FF1-devicexyz")
	assert.Contains(t, body, "reconnect")
}

func TestRootFallsBackToManualEntryOnScanError(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		Scan: func(context.Context) ([]string, error) {
			return nil, stubError("scan boom")
		},
	})
	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// No <select>; a text input for manual SSID entry instead.
	assert.NotContains(t, body, "<select")
	assert.Contains(t, body, `type="text"`)
}

func TestConnectInvokesJoinFuncWithFormValues(t *testing.T) {
	var (
		mu       sync.Mutex
		got      JoinRequest
		callHits int
	)
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Join: func(req JoinRequest) error {
			mu.Lock()
			defer mu.Unlock()
			got, callHits = req, callHits+1
			return nil
		},
	})

	resp, err := client.PostForm(ts.URL+"/connect", url.Values{
		"ssid":     {"HomeNet"},
		"password": {"s3cret-pw"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, callHits)
	assert.Equal(t, JoinRequest{SSID: "HomeNet", Password: "s3cret-pw"}, got,
		"a picker submission is neither manual nor hidden")
	// "Connecting" result page, with the reconnect hint again.
	assert.Contains(t, body, "Connecting to HomeNet")
	assert.Contains(t, body, "FF1-abc")
}

// TestConnectRoutesManualEntry pins the two-source SSID rule (D3): a non-blank
// manual field wins over the picker (the select always submits SOME value),
// carries Manual so downstream can apply the manual-only trim, and the hidden
// checkbox is honored only on this branch. The RAW manual value must pass
// through — trimming is the machine's call, keyed on the Manual flag.
func TestConnectRoutesManualEntry(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
		want JoinRequest
	}{
		{
			name: "manual entry wins over picker",
			form: url.Values{"ssid": {"PickedNet"}, "ssid_manual": {"TypedNet"},
				"password": {"pw"}, "hidden": {"1"}},
			want: JoinRequest{SSID: "TypedNet", Password: "pw", Manual: true, Hidden: true},
		},
		{
			name: "manual value passes through untrimmed",
			form: url.Values{"ssid_manual": {" TypedNet "}, "password": {"pw"}},
			want: JoinRequest{SSID: " TypedNet ", Password: "pw", Manual: true},
		},
		{
			name: "blank manual falls back to picker, hidden ignored",
			form: url.Values{"ssid": {"PickedNet"}, "ssid_manual": {"   "},
				"password": {"pw"}, "hidden": {"1"}},
			want: JoinRequest{SSID: "PickedNet", Password: "pw"},
		},
		{
			name: "no picker at all takes the manual branch even when blank",
			form: url.Values{"ssid_manual": {""}, "password": {"pw"}},
			want: JoinRequest{SSID: "", Password: "pw", Manual: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu  sync.Mutex
				got JoinRequest
			)
			_, ts, client := newTestServer(t, Config{
				Join: func(req JoinRequest) error {
					mu.Lock()
					defer mu.Unlock()
					got = req
					return nil
				},
			})
			resp, err := client.PostForm(ts.URL+"/connect", tt.form)
			require.NoError(t, err)
			_ = resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestIndexAlwaysOffersManualEntry pins the D3 fix at the template: the manual
// field and the hidden-network checkbox must render alongside a NON-empty
// picker — the old either/or template made hidden networks unprovisionable
// whenever any network was in range.
func TestIndexAlwaysOffersManualEntry(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		Scan: func(context.Context) ([]string, error) { return []string{"HomeNet"}, nil },
	})
	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)
	assert.Contains(t, body, "<select", "picker must render for the scanned networks")
	assert.Contains(t, body, `name="ssid_manual"`, "manual entry must render beside the picker")
	assert.Contains(t, body, `name="hidden"`, "hidden-network checkbox must render")
}

func TestConnectRejectionReRendersForm(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		Scan: func(context.Context) ([]string, error) { return []string{"HomeNet"}, nil },
		Join: func(req JoinRequest) error {
			return stubError("empty ssid")
		},
	})
	resp, err := client.PostForm(ts.URL+"/connect", url.Values{"ssid": {""}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Back on the picker (form action present), not the result page.
	assert.Contains(t, body, `action="/connect"`)
	assert.NotContains(t, body, "Connecting to")
}

func TestStatusReflectsStatusFunc(t *testing.T) {
	// The outcome lives outside the portal (in the machine); model that with a
	// mutable variable the StatusFunc reads.
	var (
		mu  sync.Mutex
		cur = Status{State: JoinIdle}
	)
	statusFn := func() Status {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	setStatus := func(s Status) {
		mu.Lock()
		defer mu.Unlock()
		cur = s
	}

	_, ts, client := newTestServer(t, Config{Status: statusFn})

	// Initially idle.
	assert.Equal(t, JoinIdle, fetchStatus(t, client, ts.URL).State)

	// Machine records an auth failure; /status must surface it.
	setStatus(Status{State: JoinFailed, SSID: "HomeNet", Reason: "auth-failure", Message: "Wrong password"})
	got := fetchStatus(t, client, ts.URL)
	assert.Equal(t, JoinFailed, got.State)
	assert.Equal(t, "auth-failure", got.Reason)
	assert.Equal(t, "HomeNet", got.SSID)

	// Machine records success.
	setStatus(Status{State: JoinSucceeded, SSID: "HomeNet", Message: "Connected"})
	assert.Equal(t, JoinSucceeded, fetchStatus(t, client, ts.URL).State)
}

// TestStatusPersistsAcrossServerRestart proves the outcome survives a portal
// server restart / AP re-raise: because the status is held outside the portal
// and read via StatusFunc, a brand-new Server wired to the same StatusFunc
// still reports the prior outcome.
func TestStatusPersistsAcrossServerRestart(t *testing.T) {
	var (
		mu  sync.Mutex
		cur = Status{State: JoinFailed, SSID: "HomeNet", Reason: "auth-failure", Message: "Wrong password"}
	)
	statusFn := func() Status {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}

	// First portal instance.
	s1 := NewServer(Config{Status: statusFn})
	ts1 := httptest.NewServer(s1.Handler())
	got1 := fetchStatus(t, http.DefaultClient, ts1.URL)
	assert.Equal(t, JoinFailed, got1.State)
	assert.Equal(t, "auth-failure", got1.Reason)
	ts1.Close()

	// Simulate AP re-raise: a fresh Server (new listener) with the same seam.
	s2 := NewServer(Config{Status: statusFn})
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	got2 := fetchStatus(t, http.DefaultClient, ts2.URL)
	assert.Equal(t, JoinFailed, got2.State, "outcome must survive portal restart")
	assert.Equal(t, "auth-failure", got2.Reason)
	assert.Equal(t, "HomeNet", got2.SSID)
}

func TestStartStopBindsInjectableAddr(t *testing.T) {
	s := NewServer(Config{Addr: "127.0.0.1:0", APSSID: "FF1-abc"})
	require.NoError(t, s.Start())
	defer func() { require.NoError(t, s.Stop(context.Background())) }()

	addr := s.Addr()
	assert.NotEmpty(t, addr)
	assert.True(t, strings.HasPrefix(addr, "127.0.0.1:"))

	resp, err := http.Get("http://" + addr + "/status")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStopWithoutStartIsSafe(t *testing.T) {
	s := NewServer(Config{})
	assert.NoError(t, s.Stop(context.Background()))
}

// TestConnectOversizedBodyRejected: the body-size cap must stop an oversized
// form post inside ParseForm (400) without the submission ever reaching the
// provisioning machine.
func TestConnectOversizedBodyRejected(t *testing.T) {
	joined := false
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Join:   func(JoinRequest) error { joined = true; return nil },
	})

	body := strings.NewReader("ssid=" + strings.Repeat("a", maxRequestBodyBytes+1024))
	resp, err := client.Post(ts.URL+"/connect", "application/x-www-form-urlencoded", body)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.False(t, joined, "oversized submission must never reach JoinFunc")
}

// TestInflightCapRejectsExcessRequests: with every slot held by a blocked
// handler, the next request must be shed with 429 instead of queueing a
// goroutine.
func TestInflightCapRejectsExcessRequests(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseAll := func() { once.Do(func() { close(release) }) }
	// LIFO cleanup ordering: this runs BEFORE the helper's ts.Close, so a
	// failed test cannot leave blocked handlers hanging the server shutdown.
	t.Cleanup(releaseAll)

	started := make(chan struct{}, maxInflightRequests)
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Status: func() Status {
			started <- struct{}{}
			<-release
			return Status{State: JoinIdle}
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < maxInflightRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(ts.URL + "/status")
			if err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	// Every slot is provably held by a blocked handler before the probe fires.
	for i := 0; i < maxInflightRequests; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("handlers did not saturate the in-flight cap")
		}
	}

	resp, err := client.Get(ts.URL + "/status")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

	releaseAll()
	wg.Wait()
}

// TestSlowBodyClientIsDisconnected: wire-level slowloris guard. A client that
// completes the headers but dribbles (or never sends) its POST body must be
// cut off by the server's ReadTimeout instead of retaining the connection and
// its handler goroutine — the pre-fix server bounded only the header read.
func TestSlowBodyClientIsDisconnected(t *testing.T) {
	// Compresses the package-level timeout seam; do not add t.Parallel() to
	// portal tests while this mutation pattern is in use.
	oldRead := portalReadTimeout
	portalReadTimeout = 300 * time.Millisecond
	defer func() { portalReadTimeout = oldRead }()

	joined := make(chan struct{}, 1)
	s := NewServer(Config{
		Addr:   "127.0.0.1:0",
		APSSID: "FF1-abc",
		Join:   func(JoinRequest) error { joined <- struct{}{}; return nil },
	})
	require.NoError(t, s.Start())
	defer func() { _ = s.Stop(context.Background()) }()

	conn, err := net.Dial("tcp", s.Addr())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	// Headers complete; the promised body never arrives.
	_, err = conn.Write([]byte("POST /connect HTTP/1.1\r\nHost: portal\r\n" +
		"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nssid="))
	require.NoError(t, err)

	// Drain until the server closes the connection. A read-deadline expiry here
	// means the server left the connection open past its ReadTimeout — the bug.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1024)
	for {
		if _, err = conn.Read(buf); err != nil {
			break
		}
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("server left the slow-body connection open past its ReadTimeout")
	}
	select {
	case <-joined:
		t.Fatal("partial submission must never reach JoinFunc")
	default:
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

type stubError string

func (e stubError) Error() string { return string(e) }

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func fetchStatus(t *testing.T, client *http.Client, base string) Status {
	t.Helper()
	resp, err := client.Get(base + "/status")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var st Status
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	return st
}

// --- /rescan -----------------------------------------------------------------

func TestRescanPostTriggersBounceAndExplainsRejoin(t *testing.T) {
	called := 0
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Rescan: func() error { called++; return nil },
	})

	resp, err := client.PostForm(ts.URL+"/rescan", url.Values{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, called)
	// The page must warn about the AP restart and tell the user to re-scan the
	// QR code to reconnect.
	assert.Contains(t, string(body), "FF1-abc")
	assert.Contains(t, string(body), "scan it to reconnect")
}

// TestRescanGetShowsConfirmationWithoutTriggering: the warning is a plain-HTML
// page (captive-portal mini-browsers suppress window.confirm), and merely
// viewing it must not bounce the AP.
func TestRescanGetShowsConfirmationWithoutTriggering(t *testing.T) {
	called := 0
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Rescan: func() error { called++; return nil },
	})

	resp, err := client.Get(ts.URL + "/rescan")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "Restart Wi-Fi search", "confirm page must carry the POST button")
	assert.Contains(t, body, "FF1-abc")
	assert.Contains(t, body, "disconnected")
	assert.Zero(t, called, "GET must not trigger a bounce")
}

func TestRescanOtherMethodsRedirectToPicker(t *testing.T) {
	called := 0
	_, ts, client := newTestServer(t, Config{
		Rescan: func() error { called++; return nil },
	})

	req, err := http.NewRequest(http.MethodPut, ts.URL+"/rescan", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/", resp.Header.Get("Location"))
	assert.Zero(t, called)
}

func TestRescanRejectedReRendersPicker(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Scan:   func(context.Context) ([]string, error) { return []string{"Net"}, nil },
		Rescan: func() error { return errors.New("device is busy") },
	})

	resp, err := client.PostForm(ts.URL+"/rescan", url.Values{})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Back on the picker, not the rescan page.
	assert.Contains(t, string(body), "Connect to Wi-Fi")
}
