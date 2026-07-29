package setupui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

// validContract is the player manifest the on-device player ships. Tests write
// it (or a stripped variant) to a temp file to exercise the manifest gate.
const validContract = `{
  "contracts": {
    "setupDisplay": {
      "version": 1,
      "requestKey": "request",
      "states": ["softap_qr","joining","join_failed","updating","claim_qr","ready","hidden"],
      "acceptedResponse": {"ok": true}
    }
  }
}`

// contractWithoutSetupDisplay models an older player that predates the contract.
const contractWithoutSetupDisplay = `{
  "contracts": {
    "mintPairingDisplay": {
      "version": 1,
      "requestKey": "request",
      "states": ["pairing_code","request_received","creating_token","hidden"],
      "acceptedResponse": {"ok": true}
    }
  }
}`

// fakeCDP records every setupDisplay request it is asked to send and signals a
// waiter after each call so tests can synchronize with the background worker.
type fakeCDP struct {
	mu       sync.Mutex
	err      error
	requests []map[string]any
	methods  []string
	calls    int
	signal   chan struct{}
	// gate, when non-nil, is received from at the START of every NoLogSend so
	// tests can hold the worker inside a send and control interleaving.
	gate chan struct{}
}

func newFakeCDP() *fakeCDP {
	return &fakeCDP{signal: make(chan struct{}, 64)}
}

func (f *fakeCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.calls++
	f.methods = append(f.methods, method)
	if method == cdp.METHOD_EVALUATE {
		if req, ok := parseSetupRequest(params); ok {
			f.requests = append(f.requests, req)
		}
	}
	err := f.err
	f.mu.Unlock()

	select {
	case f.signal <- struct{}{}:
	default:
	}

	if err != nil {
		return nil, err
	}
	return okResult(), nil
}

func (f *fakeCDP) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCDP) sentMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.methods))
	copy(out, f.methods)
	return out
}

func (f *fakeCDP) lastRequest() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// waitForCalls blocks until at least n CDP calls have happened, or fails the test.
func (f *fakeCDP) waitForCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for f.callCount() < n {
		select {
		case <-f.signal:
		case <-deadline:
			t.Fatalf("timed out waiting for %d CDP calls, got %d", n, f.callCount())
		}
	}
}

func parseSetupRequest(params map[string]interface{}) (map[string]any, bool) {
	expression, _ := params["expression"].(string)
	if !strings.HasPrefix(expression, "window.handleCDPRequest(") {
		return nil, false
	}
	raw := strings.TrimPrefix(expression, "window.handleCDPRequest(")
	raw = strings.TrimSuffix(raw, ")")
	var payload struct {
		Command string         `json:"command"`
		Request map[string]any `json:"request"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, false
	}
	if payload.Command != setupDisplayCommand {
		return nil, false
	}
	return payload.Request, true
}

func okResult() map[string]any {
	return map[string]any{
		"result": map[string]any{
			"result": map[string]any{
				"type":  "string",
				"value": `{"ok":true}`,
			},
		},
	}
}

func writeContract(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffos-player-contract.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func newTestService(t *testing.T, sender CDPSender, contract string) *Service {
	t.Helper()
	return New(sender, writeContract(t, contract), nil)
}

// TestTypedMethodsEmitContractPayloads asserts each typed method produces the
// correct setupDisplay state and the field presence the contract requires.
func TestTypedMethodsEmitContractPayloads(t *testing.T) {
	tests := []struct {
		name       string
		call       func(*Service)
		wantState  string
		wantFields map[string]any
		absent     []string
	}{
		{
			name:       "softap qr with password",
			call:       func(s *Service) { s.ShowSoftAPQR("FF1-abc", "secret123") },
			wantState:  stateSoftAPQR,
			wantFields: map[string]any{"ssid": "FF1-abc", "password": "secret123"},
		},
		{
			name:       "softap qr omits blank password",
			call:       func(s *Service) { s.ShowSoftAPQR("FF1-abc", "") },
			wantState:  stateSoftAPQR,
			wantFields: map[string]any{"ssid": "FF1-abc"},
			absent:     []string{"password"},
		},
		{
			name:      "scanning",
			call:      func(s *Service) { s.ShowScanning() },
			wantState: stateScanning,
			absent:    []string{"reason", "ssid", "url", "progress"},
		},
		{
			name:      "joining",
			call:      func(s *Service) { s.ShowJoining() },
			wantState: stateJoining,
			absent:    []string{"reason", "ssid", "url", "progress"},
		},
		{
			name:       "join failed with reason",
			call:       func(s *Service) { s.ShowJoinFailed("bad_password") },
			wantState:  stateJoinFailed,
			wantFields: map[string]any{"reason": "bad_password"},
		},
		{
			name:      "join failed omits blank reason",
			call:      func(s *Service) { s.ShowJoinFailed("") },
			wantState: stateJoinFailed,
			absent:    []string{"reason"},
		},
		{
			name:       "updating carries progress",
			call:       func(s *Service) { s.ShowUpdating(42) },
			wantState:  stateUpdating,
			wantFields: map[string]any{"progress": float64(42)},
		},
		{
			name:      "finalizing",
			call:      func(s *Service) { s.ShowFinalizing() },
			wantState: stateFinalizing,
			absent:    []string{"reason", "ssid", "url", "progress"},
		},
		{
			name: "claim qr carries url and device name",
			call: func(s *Service) {
				s.ShowClaimQR("https://link.feralfile.com/device_connect/xyz", "FF1-8EVTK3RE")
			},
			wantState: stateClaimQR,
			wantFields: map[string]any{
				"url":         "https://link.feralfile.com/device_connect/xyz",
				"device_name": "FF1-8EVTK3RE",
			},
		},
		{
			name:       "claim qr omits blank device name",
			call:       func(s *Service) { s.ShowClaimQR("https://link.feralfile.com/device_connect/xyz", " ") },
			wantState:  stateClaimQR,
			wantFields: map[string]any{"url": "https://link.feralfile.com/device_connect/xyz"},
			absent:     []string{"device_name"},
		},
		{
			name:      "ready",
			call:      func(s *Service) { s.ShowReady() },
			wantState: stateReady,
		},
		{
			name:      "hidden",
			call:      func(s *Service) { s.Hide() },
			wantState: stateHidden,
		},
		{
			name:      "factory reset",
			call:      func(s *Service) { s.ShowFactoryReset() },
			wantState: stateFactoryReset,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := newFakeCDP()
			svc := newTestService(t, sender, validContract)

			tt.call(svc)
			sender.waitForCalls(t, 1)

			req := sender.lastRequest()
			require.NotNil(t, req)
			assert.Equal(t, tt.wantState, req["state"])
			for k, v := range tt.wantFields {
				assert.Equal(t, v, req[k], "field %q", k)
			}
			for _, k := range tt.absent {
				_, present := req[k]
				assert.False(t, present, "field %q should be absent", k)
			}
		})
	}
}

// TestDownCDPDoesNotBlockOrPanic verifies fire-and-forget semantics against a
// player whose CDP send always fails: the caller returns immediately, nothing
// panics, and the next state change still produces a fresh push (retry-on-next-
// change). This stands in for the setup state-machine caller.
func TestDownCDPDoesNotBlockOrPanic(t *testing.T) {
	sender := newFakeCDP()
	sender.err = errors.New("cdp down")
	svc := newTestService(t, sender, validContract)

	// Each push must return promptly even though every send errors.
	done := make(chan struct{})
	go func() {
		svc.ShowJoining()
		svc.ShowJoinFailed("dhcp_timeout")
		svc.ShowUpdating(10)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Show* calls blocked on a down CDP")
	}

	// The last state change must still have been attempted against CDP.
	sender.waitForCalls(t, 1)

	// A subsequent change re-pushes (a new send attempt) despite prior failures.
	// Wait on the CONTENT, not a call count: the worker may still be draining
	// the earlier queued states, so the next send is not necessarily the ready
	// push.
	svc.ShowReady()
	assert.Eventually(t, func() bool {
		last := sender.lastRequest()
		return last != nil && last["state"] == stateReady
	}, 2*time.Second, 10*time.Millisecond, "ready state never reached CDP")
}

// TestResyncRepushesLastStateWhenCDPReturns models CDP being down for the first
// push and coming back: Resync re-sends the last intended state.
func TestResyncRepushesLastStateWhenCDPReturns(t *testing.T) {
	sender := newFakeCDP()
	sender.err = errors.New("cdp down")
	svc := newTestService(t, sender, validContract)

	svc.ShowUpdating(75)
	sender.waitForCalls(t, 1) // failed attempt

	// CDP comes back.
	sender.mu.Lock()
	sender.err = nil
	sender.mu.Unlock()

	before := sender.callCount()
	svc.Resync()
	sender.waitForCalls(t, before+1)

	last := sender.lastRequest()
	require.NotNil(t, last)
	assert.Equal(t, stateUpdating, last["state"])
	assert.Equal(t, float64(75), last["progress"])
}

// TestResyncBeforeAnyStateIsNoop guards the empty-history path.
func TestResyncBeforeAnyStateIsNoop(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)

	svc.Resync()
	// Give any erroneously-spawned worker a chance to call CDP.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, sender.callCount())
}

// TestManifestWithoutSetupDisplayDisablesNarration verifies the permanent
// no-narration fallback: an older player yields zero CDP sends, and the typed
// methods still return immediately (narration-disabled is indistinguishable
// from narration-working to the caller).
func TestManifestWithoutSetupDisplayDisablesNarration(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, contractWithoutSetupDisplay)

	svc.ShowSoftAPQR("FF1-abc", "secret123")
	svc.ShowJoining()
	svc.ShowReady()
	svc.Resync()

	// The worker resolves the manifest once and short-circuits; give it room to
	// run and confirm it never reached CDP.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, sender.callCount(), "no CDP sends when setupDisplay is unsupported")
}

// TestMissingManifestDisablesNarration covers a wholly absent contract file.
func TestMissingManifestDisablesNarration(t *testing.T) {
	sender := newFakeCDP()
	svc := New(sender, filepath.Join(t.TempDir(), "does-not-exist.json"), nil)

	svc.ShowReady()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, sender.callCount())
}

// TestFactoryResetIsSafeAgainstCurrentManifest proves the extension-state
// contract: the currently-shipping 7-state manifest (no factory_reset) still
// passes the gate, and ShowFactoryReset is delivered to CDP anyway. Requiring
// factory_reset in the gate would instead disable ALL narration on fielded
// players, so this guards against that regression.
func TestFactoryResetIsSafeAgainstCurrentManifest(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract) // validContract has the 7 shipping states, not factory_reset

	svc.ShowFactoryReset()
	sender.waitForCalls(t, 1)

	last := sender.lastRequest()
	require.NotNil(t, last)
	assert.Equal(t, stateFactoryReset, last["state"])

	// And ordinary narration still works through the same gate.
	before := sender.callCount()
	svc.ShowReady()
	sender.waitForCalls(t, before+1)
	assert.Equal(t, stateReady, sender.lastRequest()["state"])
}

func TestValidateSetupDisplayContract(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		require.NoError(t, validateSetupDisplayContract(writeContract(t, validContract)))
	})
	t.Run("missing setupDisplay", func(t *testing.T) {
		err := validateSetupDisplayContract(writeContract(t, contractWithoutSetupDisplay))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing contracts.setupDisplay")
	})
	t.Run("missing a required state", func(t *testing.T) {
		body := `{"contracts":{"setupDisplay":{"version":1,"requestKey":"request","states":["softap_qr","joining"],"acceptedResponse":{"ok":true}}}}`
		err := validateSetupDisplayContract(writeContract(t, body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "states missing")
	})
	t.Run("wrong version", func(t *testing.T) {
		body := `{"contracts":{"setupDisplay":{"version":2,"requestKey":"request","states":["softap_qr","joining","join_failed","updating","claim_qr","ready","hidden"],"acceptedResponse":{"ok":true}}}}`
		err := validateSetupDisplayContract(writeContract(t, body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version must be 1")
	})
}

// TestSendRejectionIsNonFatal ensures an {ok:false} player response is swallowed
// (logged, not returned) and does not wedge subsequent narration.
func TestSendRejectionIsNonFatal(t *testing.T) {
	sender := &rejectingCDP{signal: make(chan struct{}, 8)}
	svc := New(sender, writeContract(t, validContract), nil)

	svc.ShowReady()
	select {
	case <-sender.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("rejecting CDP was never called")
	}
	// No panic, no hang: reaching here is the assertion.
}

type rejectingCDP struct {
	signal chan struct{}
}

func (r *rejectingCDP) NoLogSend(string, map[string]interface{}) (interface{}, error) {
	select {
	case r.signal <- struct{}{}:
	default:
	}
	return map[string]any{
		"result": map[string]any{
			"result": map[string]any{"type": "string", "value": `{"ok":false,"error":"overlay busy"}`},
		},
	}, nil
}

// The production CDP client must satisfy the narrow CDPSender seam so main can
// wire it directly.
var _ CDPSender = cdp.CDP(nil)

// TestReadyThenHideDeliversBoth is the claim-flow ordering regression: the
// show=false claim path calls ShowReady() then Hide() back-to-back, and the
// player must receive BOTH (in order). The old single-slot newest-intent-wins
// queue let Hide overwrite the still-pending Ready.
func TestReadyThenHideDeliversBoth(t *testing.T) {
	fake := newFakeCDP()
	svc := newTestService(t, fake, validContract)

	svc.ShowReady()
	svc.Hide()
	fake.waitForCalls(t, 2)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.requests, 2)
	assert.Equal(t, stateReady, fake.requests[0]["state"])
	assert.Equal(t, stateHidden, fake.requests[1]["state"])
}

// TestSameStateBurstCoalesces keeps the flip side of the queue contract honest:
// a CONTIGUOUS burst of the same state (OTA progress) collapses to one trailing
// entry with the newest payload, so a slow CDP link never builds a backlog of
// stale percentages — while a repeat AFTER intervening states must append, so
// the screen ends on the newest state instead of a buried replacement.
func TestSameStateBurstCoalesces(t *testing.T) {
	svc := newTestService(t, newFakeCDP(), validContract)

	// Enqueue directly (no worker running) to test the coalescing rule itself
	// deterministically.
	svc.mu.Lock()
	svc.enqueueLocked(map[string]any{"state": stateUpdating, "progress": 10})
	svc.enqueueLocked(map[string]any{"state": stateUpdating, "progress": 50})
	svc.enqueueLocked(map[string]any{"state": stateHidden})
	svc.enqueueLocked(map[string]any{"state": stateUpdating, "progress": 90})
	queue := make([]map[string]any, len(svc.pending))
	copy(queue, svc.pending)
	svc.mu.Unlock()

	require.Len(t, queue, 3)
	assert.Equal(t, stateUpdating, queue[0]["state"])
	assert.Equal(t, 50, queue[0]["progress"], "contiguous burst must coalesce to the newest payload")
	assert.Equal(t, stateHidden, queue[1]["state"])
	assert.Equal(t, stateUpdating, queue[2]["state"])
	assert.Equal(t, 90, queue[2]["progress"], "a repeat after intervening states must append, not replace the buried entry")
}

// TestRepeatAfterInterveningStatesEndsOnNewest pins the delivery-order bug the
// old replace-in-place rule caused: softap_qr→joining→join_failed queued, then
// a fresh softap_qr (the re-raised AP). Replacing the buried first entry
// delivered softap_qr(new)→joining→join_failed and left the player on an
// obsolete failure screen while the AP was active. The queue must end on the
// newest softap_qr.
func TestRepeatAfterInterveningStatesEndsOnNewest(t *testing.T) {
	svc := newTestService(t, newFakeCDP(), validContract)

	svc.mu.Lock()
	svc.enqueueLocked(map[string]any{"state": stateSoftAPQR, "ssid": "FF1-abc", "psk": "old"})
	svc.enqueueLocked(map[string]any{"state": stateJoining})
	svc.enqueueLocked(map[string]any{"state": stateJoinFailed})
	svc.enqueueLocked(map[string]any{"state": stateSoftAPQR, "ssid": "FF1-abc", "psk": "new"})
	queue := make([]map[string]any, len(svc.pending))
	copy(queue, svc.pending)
	svc.mu.Unlock()

	require.Len(t, queue, 4)
	assert.Equal(t, stateSoftAPQR, queue[3]["state"], "delivery must END on the re-raised QR")
	assert.Equal(t, "new", queue[3]["psk"])
	assert.Equal(t, "old", queue[0]["psk"], "the earlier QR keeps its original position and payload")
}

// TestUnreadableContractDefersWithoutLatching: a read failure (boot ordering,
// OTA mid-replace of the player bundle) must NOT latch narration off for the
// process — the next push re-checks and recovers once the manifest appears.
func TestUnreadableContractDefersWithoutLatching(t *testing.T) {
	sender := newFakeCDP()
	path := filepath.Join(t.TempDir(), "contract.json")
	svc := New(sender, path, nil) // file does not exist yet

	svc.ShowJoining()
	time.Sleep(50 * time.Millisecond)
	assert.Zero(t, sender.callCount(), "no push while the contract is unreadable")

	// The player bundle lands: the very next push must recover without a
	// daemon restart.
	require.NoError(t, os.WriteFile(path, []byte(validContract), 0o600))
	svc.ShowJoining()
	sender.waitForCalls(t, 1)
}

// reloadOutcome collects a RequestPageReload done callback for test
// synchronization with the narration-lane worker.
type reloadOutcome struct {
	executed bool
	err      error
}

func awaitReload(t *testing.T, ch <-chan reloadOutcome) reloadOutcome {
	t.Helper()
	select {
	case out := <-ch:
		return out
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for RequestPageReload outcome")
		return reloadOutcome{}
	}
}

// TestRequestPageReloadExecutesWhenIdle: with no visible narration intended,
// the reload executes. Uses the no-setupDisplay contract on purpose: the
// reload is a page operation, not narration, and must work against players
// that predate the narration contract (narrationSupported must not gate it).
func TestRequestPageReloadExecutesWhenIdle(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, contractWithoutSetupDisplay)

	ch := make(chan reloadOutcome, 1)
	svc.RequestPageReload(func(executed bool, err error) {
		ch <- reloadOutcome{executed, err}
	})

	out := awaitReload(t, ch)
	assert.True(t, out.executed)
	assert.NoError(t, out.err)
	assert.Contains(t, sender.sentMethods(), "Page.reload")
}

// TestRequestPageReloadSkipsWhenNarrationIntended: narration intent present
// before the request means the reload is refused — the overlay owns the
// screen. Intent, not delivery: the no-setupDisplay contract means the
// updating push never actually painted, and the skip must still hold (the
// conservative bias Narrating documents).
func TestRequestPageReloadSkipsWhenNarrationIntended(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, contractWithoutSetupDisplay)

	svc.ShowUpdating(40)
	ch := make(chan reloadOutcome, 1)
	svc.RequestPageReload(func(executed bool, err error) {
		ch <- reloadOutcome{executed, err}
	})

	out := awaitReload(t, ch)
	assert.False(t, out.executed)
	assert.NoError(t, out.err)
	assert.NotContains(t, sender.sentMethods(), "Page.reload")
}

// TestRequestPageReloadInterleavingPreservesRacingNarration is the TOCTOU
// regression test: narration painted AFTER a reload was requested but BEFORE
// it executed must survive. The worker is held inside an earlier narration
// send while the reload is queued and an updating overlay is pushed behind
// it; when the lane drains, the reload must observe the newer intent and
// skip, and the overlay must still be delivered. This is exactly the
// interleaving a caller-side Narrating() probe followed by a raw Page.reload
// lost the race on.
func TestRequestPageReloadInterleavingPreservesRacingNarration(t *testing.T) {
	sender := newFakeCDP()
	sender.gate = make(chan struct{})
	svc := newTestService(t, sender, validContract)

	// Hold the worker inside a benign narration send (hidden: not narrating,
	// so it cannot itself cause the skip).
	svc.Hide()

	// While the worker is blocked: the recovery requests its reload, then the
	// OTA gate paints its update overlay — the reviewer's race, made
	// deterministic.
	ch := make(chan reloadOutcome, 1)
	svc.RequestPageReload(func(executed bool, err error) {
		ch <- reloadOutcome{executed, err}
	})
	svc.ShowUpdating(10)

	// Release the lane: hidden send completes, then the reload entry runs.
	sender.gate <- struct{}{} // hidden
	out := awaitReload(t, ch)
	assert.False(t, out.executed, "reload must yield to narration that raced in behind it")
	sender.gate <- struct{}{} // updating
	sender.waitForCalls(t, 2)

	assert.NotContains(t, sender.sentMethods(), "Page.reload")
	last := sender.lastRequest()
	require.NotNil(t, last)
	assert.Equal(t, "updating", last["state"], "the racing overlay must still be delivered")
}
