package otagate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeClock is a deterministic wrapper.Clock. SleepContext records the requested
// duration and advances Now instead of really sleeping, so retry-ladder and
// backoff timing can be asserted without wall-clock waits.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
	// sleepErr, when set, is returned by SleepContext (models ctx cancellation).
	sleepErr error
	// onSleep, when set, runs after each recorded sleep (outside the lock) so a
	// test can count polls or cancel a ctx after N iterations.
	onSleep func()
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(d time.Duration) { _ = c.SleepContext(context.Background(), d) }

func (c *fakeClock) SleepContext(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	if c.sleepErr != nil {
		c.mu.Unlock()
		return c.sleepErr
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	cb := c.onSleep
	c.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}

func (c *fakeClock) recordedSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.sleeps))
	copy(out, c.sleeps)
	return out
}

func (c *fakeClock) NewTicker(d time.Duration) wrapper.Ticker { return &fakeTicker{d: d} }

type fakeTicker struct{ d time.Duration }

func (t *fakeTicker) Reset(d time.Duration) { t.d = d }
func (t *fakeTicker) Stop()                 {}
func (t *fakeTicker) C() <-chan time.Time   { return make(chan time.Time) }

// fakeHTTP is a wrapper.HTTPClient whose Do is driven by an injected handler.
type fakeHTTP struct {
	mu   sync.Mutex
	do   func(req *http.Request) (*http.Response, error)
	call int
}

func (h *fakeHTTP) NewRequest(method, url string, body io.Reader) (*http.Request, error) {
	return http.NewRequest(method, url, body)
}

func (h *fakeHTTP) Do(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.call++
	h.mu.Unlock()
	return h.do(req)
}

func (h *fakeHTTP) Get(url string) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	return h.Do(req)
}

func (h *fakeHTTP) Post(_ string, _ string, _ io.Reader) (*http.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func (h *fakeHTTP) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.call
}

// jsonResponse builds a 200 response carrying the given body.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// okManifest is a well-formed distributor manifest for the given versions.
func okManifest(minRuntime, minUpgradeable, latest string) string {
	return fmt.Sprintf(
		`{"min_runtime_version":%q,"min_upgradeable_version":%q,"flashing_guide":"https://guide","latest_version":%q}`,
		minRuntime, minUpgradeable, latest)
}

// fakeConfig is a ConfigProvider returning canned local-build values.
type fakeConfig struct {
	branch   string
	version  string
	endpoint string
	err      error
}

func (c fakeConfig) LocalBuild(context.Context) (string, string, string, error) {
	return c.branch, c.version, c.endpoint, c.err
}

// fakeRunner is an UpdateRunner returning a scripted sequence of errors. Each
// Run call consumes the next entry (nil == success). Calls beyond the script
// reuse the last entry. It optionally blocks on gate to model an in-flight update.
type fakeRunner struct {
	mu      sync.Mutex
	results []error
	call    int
	// emit, when non-nil, is delivered to Run's onProgress callback in order
	// (before the scripted result is returned), modeling the updater's progress
	// lines. Each entry is a percent, or -1 for a line with no percent field.
	emit []int
	// gate, when non-nil, is received-from before each Run returns; entered is
	// closed on the first Run entry so a test can synchronize on it.
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (r *fakeRunner) Run(_ context.Context, onProgress func(int, string)) error {
	r.mu.Lock()
	idx := r.call
	r.call++
	emit := r.emit
	r.mu.Unlock()

	if r.entered != nil {
		r.once.Do(func() { close(r.entered) })
	}
	if r.gate != nil {
		<-r.gate
	}

	if onProgress != nil {
		for _, pct := range emit {
			onProgress(pct, "")
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) == 0 {
		return nil
	}
	if idx >= len(r.results) {
		idx = len(r.results) - 1
	}
	return r.results[idx]
}

func (r *fakeRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.call
}

// transientRunErr / permanentRunErr are runner errors whose messages classify
// (via classifyUpdaterMessage) as transient/permanent respectively.
func transientRunErr() error { return fmt.Errorf("No network connection. Aborting update.") }
func permanentRunErr() error { return fmt.Errorf("airootfs.sfs not found in image.") }

// fakeExec is a scripted wrapper.Exec: it records every command line and fails
// commands whose argv contains a configured substring. Unmatched commands
// succeed with empty output. Kept hand-rolled (like the other fakes here) so
// runner tests stay free of gomock ceremony.
type fakeExec struct {
	mu   sync.Mutex
	cmds []string
	// fail maps an argv substring to the error its Run/CombinedOutput returns.
	fail map[string]error
}

func (e *fakeExec) CommandContext(_ context.Context, name string, arg ...string) wrapper.ExecCmd {
	line := strings.Join(append([]string{name}, arg...), " ")
	e.mu.Lock()
	e.cmds = append(e.cmds, line)
	e.mu.Unlock()
	return fakeCmd{line: line, exec: e}
}

func (e *fakeExec) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.cmds))
	copy(out, e.cmds)
	return out
}

func (e *fakeExec) errFor(line string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for sub, err := range e.fail {
		if strings.Contains(line, sub) {
			return err
		}
	}
	return nil
}

type fakeCmd struct {
	line string
	exec *fakeExec
}

func (c fakeCmd) String() string                  { return c.line }
func (c fakeCmd) Run() error                      { return c.exec.errFor(c.line) }
func (c fakeCmd) Start() error                    { return c.exec.errFor(c.line) }
func (c fakeCmd) Wait() error                     { return nil }
func (c fakeCmd) Output() ([]byte, error)         { return nil, c.exec.errFor(c.line) }
func (c fakeCmd) CombinedOutput() ([]byte, error) { return nil, c.exec.errFor(c.line) }
