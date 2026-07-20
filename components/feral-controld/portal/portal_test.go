package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

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
		gotSSID  string
		gotPass  string
		callHits int
	)
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Join: func(ssid, password string) error {
			mu.Lock()
			defer mu.Unlock()
			gotSSID, gotPass, callHits = ssid, password, callHits+1
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
	assert.Equal(t, "HomeNet", gotSSID)
	assert.Equal(t, "s3cret-pw", gotPass)
	// "Connecting" result page, with the reconnect hint again.
	assert.Contains(t, body, "Connecting to HomeNet")
	assert.Contains(t, body, "FF1-abc")
}

func TestConnectRejectionReRendersForm(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		Scan: func(context.Context) ([]string, error) { return []string{"HomeNet"}, nil },
		Join: func(ssid, password string) error {
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
