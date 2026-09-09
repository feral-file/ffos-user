package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
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
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

func TestRootRendersEssentialNetworkPicker(t *testing.T) {
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
	// HTML is dynamic (scan list, join status): never cached, or a CNA back
	// navigation would show a stale picker.
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Contains(t, body, "HomeNet")
	assert.Contains(t, body, "Cafe-5G")
	assert.Contains(t, body, "Choose a Wi-Fi network")
	// Copy must hold in both form shapes (picker and empty-scan manual
	// entry), so it never says "selected".
	assert.Contains(t, body, "Use that network's own password—not the setup password.")
	assert.Contains(t, body, ">Connect</button>")
	assert.Contains(t, body, ">Network not listed?</button>")
	assert.NotContains(t, body, "FF1-devicexyz", "the setup SSID is not destination guidance")
	assert.NotContains(t, body, "Don't see your Wi-Fi network", "the refresh confirmation owns that explanation")
}

func TestRootOmitsSetupNetworkAndRequiresAVisibleChoice(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-devicexyz",
		Scan: func(context.Context) ([]string, error) {
			return []string{"FF1-devicexyz", "HomeNet"}, nil
		},
	})
	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)

	assert.Equal(t, 0, strings.Count(body, "FF1-devicexyz"),
		"the active setup SSID must never be offered or presented as the destination")
	assert.Contains(t, body, `<option value="" selected disabled>Select a network…</option>`,
		"the first scanned network must not be silently selected")
	assert.Contains(t, body, `<select id="ssid" name="ssid">`)
	assert.NotContains(t, body, `<select id="ssid" name="ssid" required>`,
		"base HTML must allow the visible manual field to submit without JavaScript")
	assert.Contains(t, body, "select.required = !other;",
		"JavaScript-capable sheets should still validate the active picker branch")
	assert.Contains(t, body, `<option value="HomeNet">HomeNet</option>`)
}

func TestRootFiltersSetupNetworkBeforeDisplayCap(t *testing.T) {
	destinations := []string{
		"Net1", "Net2", "Net3", "Net4", "Net5",
		"Net6", "Net7", "Net8", "Net9",
	}
	require.Len(t, destinations, maxDisplayedSSIDs)
	scan := append([]string{"FF1-devicexyz"}, destinations...)
	scan = append(scan, "Net10")

	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-devicexyz",
		Scan: func(context.Context) ([]string, error) {
			return scan, nil
		},
	})
	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)

	assert.NotContains(t, body, "FF1-devicexyz")
	for _, ssid := range destinations {
		assert.Contains(t, body, `<option value="`+ssid+`">`+ssid+`</option>`)
	}
	assert.NotContains(t, body, "Net10",
		"the display cap still applies after the setup network is removed")
}

func TestManualSSIDOptionCannotCollideWithARealSSID(t *testing.T) {
	assert.Greater(t, len(manualSSIDOption), 32)
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
// manual field wins over any picker value, carries Manual so downstream can
// apply the manual-only trim, and honors the hidden checkbox only on this
// branch. The RAW manual value must pass through — trimming is the machine's
// call, keyed on the Manual flag.
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
		{
			name: "manual option sentinel becomes an empty manual value",
			form: url.Values{"ssid": {manualSSIDOption}, "ssid_manual": {""}, "password": {"pw"}},
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
	// The page script collapses manual entry until this option is picked, so
	// dropping it would make hidden networks unprovisionable again for every
	// JS-enabled phone. The impossible-SSID sentinel routes to the manual branch
	// while no-JS phones can type directly into the always-visible field.
	assert.Contains(t, body, `<option value="`+manualSSIDOption+`" data-manual>Other network…</option>`,
		"picker must carry the manual-entry escape option")
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
	// The rejection reason is IN the page. Load-bearing for the no-JS shape:
	// the machine never saw the submission (Status stays JoinIdle), and the
	// script-side `required` guard is the only other thing preventing an
	// empty placeholder-first submission — without this banner a
	// scripting-disabled sheet re-renders a byte-identical page and the tap
	// silently did nothing.
	assert.Contains(t, body, "empty ssid")
}

// TestConnectEmptyPickerSubmissionExplainsItself is the no-JS regression for
// the placeholder-first picker: an untouched picker submits ssid="" (the
// disabled prompt), which must come back with the machine's user-facing
// rejection visible, not a silently identical page. Uses the real
// provisioning-side message so the copy contract is pinned end-to-end.
func TestConnectEmptyPickerSubmissionExplainsItself(t *testing.T) {
	_, ts, client := newTestServer(t, Config{
		Scan: func(context.Context) ([]string, error) { return []string{"HomeNet"}, nil },
		Join: func(req JoinRequest) error {
			if strings.TrimSpace(req.SSID) == "" {
				return stubError("please choose a Wi-Fi network")
			}
			return nil
		},
	})
	resp, err := client.PostForm(ts.URL+"/connect", url.Values{"ssid": {""}, "ssid_manual": {""}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body := readAll(t, resp)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "please choose a Wi-Fi network")
	assert.Contains(t, body, `class="status status-failed"`)
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
	core, observed := observer.New(zap.InfoLevel)
	_, ts, client := newTestServer(t, Config{
		APSSID: "FF1-abc",
		Logger: zap.New(core),
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
	// A shed request writes no access line: the cap's own saturation is the
	// evidence there, and logging what it rejects is what would let a
	// sequential stream drive the log (review bot on 6ba6f96).
	assert.Equal(t, maxInflightRequests, observed.FilterMessage("portal: request").Len(),
		"only the admitted requests logged")

	releaseAll()
	wg.Wait()
}

// TestAccessLineTruncatesClientControlledFields: the path and User-Agent of
// the access line are attacker-controlled and unbounded on the wire, so the
// line carries bounded copies of both.
func TestAccessLineTruncatesClientControlledFields(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	h := NewServer(Config{APSSID: "FF1-abc", Logger: zap.New(core)}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/"+strings.Repeat("a", 4000), nil)
	req.Header.Set("User-Agent", strings.Repeat("u", 4000))
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := observed.FilterMessage("portal: request").All()
	require.Len(t, lines, 1)
	fields := lines[0].ContextMap()
	path, _ := fields["path"].(string)
	agent, _ := fields["user_agent"].(string)
	assert.Equal(t, maxLoggedPathBytes+len("…"), len(path), "the path is cut at its byte bound")
	assert.True(t, strings.HasSuffix(path, "…"), "a cut value is marked")
	assert.Equal(t, maxLoggedUserAgentBytes+len("…"), len(agent), "the agent is cut at its byte bound")
	assert.True(t, strings.HasSuffix(agent, "…"))
	assert.NotContains(t, fields, "suppressed", "nothing was dropped")

	// A field inside the bound passes through whole.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", nil))
	lines = observed.FilterMessage("portal: request").All()
	require.Len(t, lines, 2)
	assert.Equal(t, "/status", lines[1].ContextMap()["path"])
}

// TestAccessLineIsRateLimitedAndReportsWhatItDropped: a client on the open
// setup subnet can send a sequential stream, and controld.log rotates on time
// rather than size — so the line is capped at accessLogBurst with an
// accessLogPerMinute refill, and the flood it swallowed is reported as a
// count on the next line rather than as silence.
func TestAccessLineIsRateLimitedAndReportsWhatItDropped(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)
	s := NewServer(Config{APSSID: "FF1-abc", Logger: zap.New(core)})
	now := time.Now()
	s.access.now = func() time.Time { return now }
	h := s.Handler()
	get := func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", nil))
	}
	lines := func() int { return observed.FilterMessage("portal: request").Len() }

	for i := 0; i < accessLogBurst; i++ {
		get()
	}
	require.Equal(t, accessLogBurst, lines(), "the burst covers a whole setup session")

	const flood = 25
	for i := 0; i < flood; i++ {
		get()
	}
	assert.Equal(t, accessLogBurst, lines(), "the burst spent, a stream of requests writes nothing")

	// A minute of refill later the line resumes, carrying what the flood cost.
	now = now.Add(time.Minute)
	get()
	all := observed.FilterMessage("portal: request").All()
	require.Len(t, all, accessLogBurst+1)
	assert.EqualValues(t, flood, all[len(all)-1].ContextMap()["suppressed"],
		"a flood shows up as one number, not as silence")

	// The count is claimed exactly once.
	get()
	all = observed.FilterMessage("portal: request").All()
	require.Len(t, all, accessLogBurst+2)
	assert.NotContains(t, all[len(all)-1].ContextMap(), "suppressed")
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
	// The transient page only needs the next action; the confirmation already
	// explained why the phone will disconnect.
	assert.Contains(t, string(body), "When the QR code returns on your Art Computer, scan it again.")
	assert.NotContains(t, string(body), "FF1-abc")
	assert.NotContains(t, string(body), "see the updated list")
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
	assert.Contains(t, body, `<form method="POST" action="/rescan">`, "confirm page must carry the POST button")
	assert.Contains(t, body, "Refresh network list", "confirm button must name the disruptive operation")
	assert.Contains(t, body, "briefly disconnects your phone")
	assert.Contains(t, body, "scan it again")
	// The two causes the entry button names — hidden networks and dense
	// environments — are not fixed by a rescan, so the confirm page must
	// point at the escape that does fix them before the disruptive bounce.
	assert.Contains(t, body, "type its name instead",
		"confirm page must name the manual-entry escape for hidden/dense-scan networks")
	assert.NotContains(t, body, "FF1-abc")
	assert.NotContains(t, body, "see the updated list")
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
	assert.Contains(t, string(body), "Choose a Wi-Fi network")
}

// TestFontsServeEmbeddedFaces: the /fonts/ subtree must serve the embedded
// woff2 files (the portal's PP Mori faces) rather than falling into
// handleRoot's treat-unknown-paths-as-captive-probes redirect, and must 404 —
// not redirect — for anything else under it, including traversal shapes.
func TestFontsServeEmbeddedFaces(t *testing.T) {
	_, ts, client := newTestServer(t, Config{APSSID: "FF1-abc"})

	for _, name := range []string{"PPMori-Regular.woff2", "PPMori-Bold.woff2"} {
		resp, err := client.Get(ts.URL + "/fonts/" + name)
		require.NoError(t, err)
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		_ = resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode, name)
		assert.Equal(t, "font/woff2", resp.Header.Get("Content-Type"), name)
		// Session-scale only: the URLs carry no fingerprint, so anything longer
		// would pin a stale face across an OTA font swap.
		assert.Equal(t, "max-age=3600", resp.Header.Get("Cache-Control"), name)
		assert.NotEmpty(t, body, name)
	}

	for _, path := range []string{
		"/fonts/",
		"/fonts/missing.woff2",
		"/fonts/PPMori-Regular.ttf",          // only woff2 is embedded
		"/fonts/sub/PPMori-Regular.woff2",    // no subdirectories
		"/fonts/%2e%2e/templates/index.html", // encoded traversal
	} {
		resp, err := client.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
	}
}

// TestSetupCSSServed: every template now links /setup.css instead of carrying
// its own <style> block, so the route must serve real CSS (not bounce to the
// captive-probe redirect) and every page must actually reference it.
func TestSetupCSSServed(t *testing.T) {
	_, ts, client := newTestServer(t, Config{APSSID: "FF1-abc"})

	resp, err := client.Get(ts.URL + "/setup.css")
	require.NoError(t, err)
	body, readErr := io.ReadAll(resp.Body)
	require.NoError(t, readErr)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/css; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "max-age=3600", resp.Header.Get("Cache-Control"))
	assert.Contains(t, string(body), "PP Mori")

	// Every template must link the shared sheet — checked against the embedded
	// sources rather than by rendering each route, so a future page cannot
	// slip in unstyled regardless of how it is reached.
	names, err := fs.Glob(assets, "templates/*.html")
	require.NoError(t, err)
	require.NotEmpty(t, names)
	for _, name := range names {
		raw, readErr := assets.ReadFile(name)
		require.NoError(t, readErr)
		assert.Contains(t, string(raw), `href="/setup.css"`, name)
		assert.NotContains(t, string(raw), "<style>", name)
	}
}

// TestActivityObservedOnHumanRoutesOnly pins the §4.2 deferral signal's
// classification: the /connect and /rescan action handlers count (by handler,
// not method — the rescan form submits as GET), while root fetches and OS
// captive probes never do, because the probe routes exist precisely to
// redirect an idle phone's automatic requests to "/" and counting them would
// let an idle phone pin every teardown to its ceiling.
func TestActivityObservedOnHumanRoutesOnly(t *testing.T) {
	var observed int
	var mu sync.Mutex
	_, ts, client := newTestServer(t, Config{
		Join:             func(JoinRequest) error { return nil },
		Rescan:           func() error { return nil },
		ActivityObserved: func() { mu.Lock(); observed++; mu.Unlock() },
	})

	count := func() int { mu.Lock(); defer mu.Unlock(); return observed }

	resp, err := client.Get(ts.URL + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	resp, err = client.Get(ts.URL + "/generate_204")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 0, count(), "root fetches and OS probes are not human actions")

	resp, err = client.PostForm(ts.URL+"/connect", url.Values{"ssid": {"X"}})
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 1, count(), "a credential submit is a human action")

	resp, err = client.Get(ts.URL + "/rescan")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 2, count(), "the rescan confirmation page is handler-classified, method-agnostic")

	resp, err = client.PostForm(ts.URL+"/rescan", nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, 3, count())
}

// TestTrafficObservedCountsEveryRequest pins the two observers' opposite
// classifications: TrafficObserved fires for EVERY request — root fetches and
// OS captive probes included — while ActivityObserved stays action-only. The
// recheck cadence's attached-phone deferral depends on exactly this split.
func TestTrafficObservedCountsEveryRequest(t *testing.T) {
	var traffic, activity int
	var mu sync.Mutex
	count := func(n *int) func() {
		return func() { mu.Lock(); *n++; mu.Unlock() }
	}
	_, ts, client := newTestServer(t, Config{
		APSSID:           "FF1-abc",
		Scan:             func(context.Context) ([]string, error) { return []string{"Net"}, nil },
		Rescan:           func() error { return nil },
		ActivityObserved: count(&activity),
		TrafficObserved:  func(ClientKind) { count(&traffic)() },
	})

	// The asset routes ride along: a browser auto-fetching the stylesheet or a
	// font proves a device is attached but is never a human action.
	for _, path := range []string{
		"/", "/generate_204", "/hotspot-detect.html",
		"/setup.css", "/fonts/PPMori-Regular.woff2",
	} {
		resp, err := client.Get(ts.URL + path)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	mu.Lock()
	assert.Equal(t, 5, traffic, "probes, root fetches, and asset fetches all count as traffic")
	assert.Equal(t, 0, activity, "none of them count as human activity")
	mu.Unlock()

	resp, err := client.Get(ts.URL + "/rescan")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	mu.Lock()
	assert.Equal(t, 6, traffic)
	assert.Equal(t, 1, activity, "an action request counts as both")
	mu.Unlock()
}

// TestIndexCarriesHandOffWatcher pins the picker's /status watcher (the
// #3515 Safari hand-off): the page must poll /status and be able to turn
// itself into the hand-off screen on a started join; a failed join never
// hands off — it reloads an untouched picker for the banner and leaves a
// touched one alone.
func TestIndexCarriesHandOffWatcher(t *testing.T) {
	srv := NewServer(Config{APSSID: "FF1-abc"})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	assert.Contains(t, body, "fetch('/status'")
	assert.Contains(t, body, "st.state === 'joining' || st.state === 'succeeded'")
	assert.Contains(t, body, "Setup continues on your Art Computer")
	// A failed join reaching an UNTOUCHED picker reloads it so the server's
	// failure banner appears (Safari was frozen under the sheet for the whole
	// wrong-password round trip); a form holding input or focus stays.
	assert.Contains(t, body, "if (st && st.state === 'failed' && renderedStatus !== 'failed' && !formTouched()) {")
	assert.Contains(t, body, `<main data-status="idle">`, "an idle render stamps idle")
	// The shapes the 2026-09-07 trials and audit forced (see the template
	// comment): an immediate first poll, a bounded fetch, a visibility hook,
	// the watcher header, HTTP errors not counted as misses, the form guard,
	// and the post-hand-off return watch that reloads into the picker.
	assert.Contains(t, body, "if (!main || !window.fetch || !window.AbortController) return;",
		"no watcher without a bounded fetch — an unbounded poll would hang the return watch")
	assert.Contains(t, body, "new AbortController()")
	assert.NotContains(t, body, "return fetch('/status', opts);", "no unbounded fallback")
	assert.Contains(t, body, "visibilitychange")
	assert.Contains(t, body, "if (handedOff) opts.headers = { 'X-Setup-Watcher': '1' };",
		"only post-hand-off polls are excluded from traffic; an open picker still counts as a human mid-setup")
	assert.Contains(t, body, "if (!r.ok) { setTimeout(poll, 2000); return; }")
	assert.Contains(t, body, "misses >= 3 && !formTouched()")
	assert.Contains(t, body, "window.location.reload()")
	// html/template elides JS comments in the served body, so the prose
	// mention of the removed gate never reaches the client; pin the CODE.
	assert.NotContains(t, body, "var answered", "a page served by the device needs no answered gate")
	assert.Regexp(t, `\n    poll\(\);\n  \}\)\(\);`, body, "the first poll must run on load, not on a timer")
}

// TestWatcherPollsAreNotTraffic: the picker's post-hand-off /status watcher
// must not register as an attached device talking — it would pin the
// recheck deferral and the address-QR phase open on its own. (Pre-hand-off
// polls carry no header and count; see the template.)
func TestWatcherPollsAreNotTraffic(t *testing.T) {
	var mu sync.Mutex
	traffic := 0
	h := NewServer(Config{APSSID: "FF1-abc", TrafficObserved: func(ClientKind) {
		mu.Lock()
		traffic++
		mu.Unlock()
	}}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set(watcherHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "the watcher poll is still served")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", nil))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, traffic, "only the unmarked /status counted")
}

// TestTrafficObservedClassifiesAppleClients pins the ClientKind the seam
// hands the machine: the iOS/macOS probe agent is Apple, everything else —
// Android's generic desktop-looking probe agent included — is unknown.
func TestTrafficObservedClassifiesAppleClients(t *testing.T) {
	var mu sync.Mutex
	var kinds []ClientKind
	h := NewServer(Config{APSSID: "FF1-abc", TrafficObserved: func(k ClientKind) {
		mu.Lock()
		kinds = append(kinds, k)
		mu.Unlock()
	}}).Handler()
	for _, ua := range []string{
		"CaptiveNetworkSupport-514.160.1.0.1 wispr",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/60.0.3112.32 Safari/537.36",
		"Dalvik/2.1.0 (Linux; U; Android 17; Pixel 8 Build/BP1A)",
		"",
	} {
		req := httptest.NewRequest(http.MethodGet, "/hotspot-detect.html", nil)
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []ClientKind{ClientApple, ClientUnknown, ClientUnknown, ClientUnknown}, kinds)
}

// TestFailedPickerRenderStampsItsStatus: the picker rendered WITH the failure
// banner must stamp data-status="failed" so its watcher does not reload
// again on the same persisted outcome (the reload loop the review bot caught).
func TestFailedPickerRenderStampsItsStatus(t *testing.T) {
	srv := NewServer(Config{APSSID: "FF1-abc", Status: func() Status {
		return Status{State: JoinFailed, SSID: "Home", Reason: "auth-failure", Message: "Wrong Wi-Fi password."}
	}})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	assert.Contains(t, body, `<main data-status="failed">`)
	assert.Contains(t, body, "Wrong Wi-Fi password.", "the banner the reload exists to show")
}
