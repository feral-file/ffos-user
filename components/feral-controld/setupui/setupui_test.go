package setupui

import (
	"context"
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
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
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

// contractWithConnecting models the current shipping manifest, which lists the
// connecting extension state (validContract deliberately predates it, so the
// pair exercises ShowConnecting's downgrade gate).
const contractWithConnecting = `{
  "contracts": {
    "setupDisplay": {
      "version": 1,
      "requestKey": "request",
      "states": ["softap_qr","joining","join_failed","connecting","updating","claim_qr","ready","hidden"],
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
	// navigating models the document-replacement window a playersession
	// recovery navigation (Page.navigate) opens: while set, narration
	// evaluates execute in the DYING context — they are counted in
	// lostEvaluates and fail, never reaching requests. It is set by a
	// Page.navigate send and cleared once readyAfterProbes readiness probes
	// have been answered (the probe that crosses the threshold observes the
	// replacement document).
	navigating       bool
	readyAfterProbes int
	probeCount       int
	lostEvaluates    int
	// stampedNonce is the navigation nonce the stamp evaluate wrote into the
	// "old document"; while navigating, a probe reads false only when it
	// carries THIS value (a probe with a different or absent nonce is fooled
	// by the old document — the HIGH-severity hazard the nonce exists to
	// prevent).
	stampedNonce string
}

func newFakeCDP() *fakeCDP {
	return &fakeCDP{signal: make(chan struct{}, 64)}
}

// NoLogSend mimics the REAL cdp client's post-processed contract, not the raw
// wire envelope: evaluate results arrive already decoded (a JSON-string
// expression comes back as its unmarshaled map), and shapes the client cannot
// decode are errors. Divergence here previously let an on-device-inert
// readiness probe pass a green suite — keep this fake honest against
// cdp.send's switch when adding evaluate shapes.
func (f *fakeCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.calls++
	f.methods = append(f.methods, method)
	if method == "Page.navigate" {
		f.navigating = true
	}
	var result interface{} = map[string]any{"ok": true}
	err := f.err
	if method == cdp.METHOD_EVALUATE {
		expression, _ := params["expression"].(string)
		switch {
		case !strings.HasPrefix(expression, "JSON.stringify(") &&
			!strings.HasPrefix(expression, "window.handleCDPRequest("):
			// Structural guard, not just documentation: cdp.send decodes only
			// type:"string" (JSON-unmarshaled) and type:"object" evaluate
			// results and ERRORS on everything else, so any expression this
			// package sends must evaluate to a JSON string or an object. A
			// bare-boolean probe — the exact on-device-inert defect a prior
			// round shipped — fails here instead of passing green.
			err = errors.New("CDP response type mismatch: boolean")
		case strings.HasPrefix(expression, "JSON.stringify({nonce:"):
			// The reload nonce stamp: record what the "old document" now holds
			// so probes can be answered the way a real dying document would.
			if start := strings.Index(expression, `= "`); start >= 0 {
				rest := expression[start+len(`= "`):]
				if end := strings.Index(rest, `"`); end >= 0 {
					f.stampedNonce = rest[:end]
				}
			}
			result = map[string]any{"nonce": f.stampedNonce}
		case strings.Contains(expression, "typeof window.handleCDPRequest"):
			// Readiness probe. The dying document runs the SAME player app, so
			// it answers handler-presence questions too: while navigating, a
			// probe reads false only when it carries the exact stamped nonce —
			// a probe with a different or absent nonce is fooled by the old
			// document and reports ready, which downstream assertions surface
			// as lost narration.
			f.probeCount++
			if f.navigating && f.probeCount > f.readyAfterProbes {
				f.navigating = false // navigation commits; replacement document answers now
			}
			ready := true
			if f.navigating {
				ready = f.stampedNonce == "" || !strings.Contains(expression, f.stampedNonce)
			}
			result = map[string]any{"ready": ready}
		case f.navigating:
			// A narration evaluate landed in the document being torn down: it
			// never reaches the replacement page's handler.
			f.lostEvaluates++
			err = errors.New("Execution context was destroyed")
		default:
			if req, ok := parseSetupRequest(params); ok {
				f.requests = append(f.requests, req)
			}
		}
	}
	f.mu.Unlock()

	select {
	case f.signal <- struct{}{}:
	default:
	}

	if err != nil {
		return nil, err
	}
	return result, nil
}

// Initialized satisfies playersession.CDPSender for the setupui↔playersession
// integration test; this fake is always "connected".
func (f *fakeCDP) Initialized() bool { return true }

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

// TestShowConnectingManifestGate: ShowConnecting emits the connecting state
// when the player manifest lists it and downgrades to join_failed when the
// manifest predates it — the downgrade (not a renders-nothing no-op) is what
// keeps the M-0/M-1 offline narration visible on older fielded players.
func TestShowConnectingManifestGate(t *testing.T) {
	t.Run("manifest lists connecting", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, contractWithConnecting)

		svc.ShowConnecting("Checking the network connection…")
		sender.waitForCalls(t, 1)

		req := sender.lastRequest()
		require.NotNil(t, req)
		assert.Equal(t, stateConnecting, req["state"])
		assert.Equal(t, "Checking the network connection…", req["reason"])
	})

	t.Run("blank message omits reason", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, contractWithConnecting)

		svc.ShowConnecting(" ")
		sender.waitForCalls(t, 1)

		req := sender.lastRequest()
		require.NotNil(t, req)
		assert.Equal(t, stateConnecting, req["state"])
		_, present := req["reason"]
		assert.False(t, present, "blank message must omit the reason field")
	})

	t.Run("older manifest downgrades to join_failed", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.ShowConnecting("Checking the network connection…")
		sender.waitForCalls(t, 1)

		req := sender.lastRequest()
		require.NotNil(t, req)
		assert.Equal(t, stateJoinFailed, req["state"])
		assert.Equal(t, "Checking the network connection…", req["reason"])
	})

	// The boot-race regression: the hedge fires seconds after daemon start,
	// while the player bundle can be unreadable (boot ordering, OTA
	// mid-replace). The push must defer UN-downgraded, so that once the
	// manifest lands and CDP reconnects, the Resync replay delivers the
	// neutral state — a Show-time downgrade frozen into the retained intent
	// would replay the false join_failed flash this state exists to remove.
	t.Run("unreadable manifest defers; replay delivers connecting once readable", func(t *testing.T) {
		sender := newFakeCDP()
		path := filepath.Join(t.TempDir(), "contract.json")
		svc := New(sender, path, nil) // manifest does not exist yet

		svc.ShowConnecting("Looking for your Wi-Fi network…")
		time.Sleep(50 * time.Millisecond)
		assert.Zero(t, sender.callCount(), "no push while the contract is unreadable")

		require.NoError(t, os.WriteFile(path, []byte(contractWithConnecting), 0o600))
		svc.Resync()
		sender.waitForCalls(t, 1)

		req := sender.lastRequest()
		require.NotNil(t, req)
		assert.Equal(t, stateConnecting, req["state"])
		assert.Equal(t, "Looking for your Wi-Fi network…", req["reason"])
	})

	// The player bundle (and its manifest) is OTA-replaced without a controld
	// restart, so the capability verdict must never latch for the process
	// lifetime. Both staleness directions:
	t.Run("bundle upgrade after a downgrade verdict delivers connecting on replay", func(t *testing.T) {
		sender := newFakeCDP()
		path := filepath.Join(t.TempDir(), "contract.json")
		require.NoError(t, os.WriteFile(path, []byte(validContract), 0o600))
		svc := New(sender, path, nil)

		svc.ShowConnecting("Checking the network connection…")
		sender.waitForCalls(t, 1)
		require.Equal(t, stateJoinFailed, sender.lastRequest()["state"])

		// The bundle updates in place; a latched supportNo would keep
		// downgrading on the upgraded player until a daemon restart.
		require.NoError(t, os.WriteFile(path, []byte(contractWithConnecting), 0o600))
		svc.Resync()
		sender.waitForCalls(t, 2)
		assert.Equal(t, stateConnecting, sender.lastRequest()["state"])
	})

	t.Run("unreadable manifest after an old-player verdict keeps the downgrade", func(t *testing.T) {
		sender := newFakeCDP()
		path := filepath.Join(t.TempDir(), "contract.json")
		require.NoError(t, os.WriteFile(path, []byte(validContract), 0o600))
		svc := New(sender, path, nil)

		svc.ShowConnecting("Checking the network connection…")
		sender.waitForCalls(t, 1)
		require.Equal(t, stateJoinFailed, sender.lastRequest()["state"])

		// narrationSupported's overall verdict latched supportYes on the push
		// above, so the send path stays open when the manifest then goes
		// unreadable (OTA mid-replace). The replay must fall back to the last
		// verdict read from a real manifest — an un-downgraded connecting
		// would be a renders-nothing no-op on the old player still on screen.
		require.NoError(t, os.Remove(path))
		svc.Resync()
		sender.waitForCalls(t, 2)
		assert.Equal(t, stateJoinFailed, sender.lastRequest()["state"])
	})
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

// TestResync_NoOpWhenPendingNonEmpty pins minor #14: Resync now also runs as
// a generation-ready reconciler (on every document replacement, not just the
// original CDP on-connect wiring), so it can fire while a genuine
// multi-state sequence (e.g. the claim flow's ShowReady()+Hide() — two
// DISTINCT states that must both reach the player) is still queued. It must
// leave a non-empty queue alone rather than collapsing it down to just the
// last intent, which would silently drop the Ready.
func TestResync_NoOpWhenPendingNonEmpty(t *testing.T) {
	svc := newTestService(t, newFakeCDP(), validContract)

	svc.mu.Lock()
	svc.last = map[string]any{"state": stateHidden}
	svc.pending = []map[string]any{
		{"state": stateReady},
		{"state": stateHidden},
	}
	svc.running = true // pretend a worker is already draining this queue
	svc.mu.Unlock()

	svc.Resync()

	svc.mu.Lock()
	defer svc.mu.Unlock()
	require.Len(t, svc.pending, 2, "a non-empty queue must be left alone")
	assert.Equal(t, stateReady, svc.pending[0]["state"])
	assert.Equal(t, stateHidden, svc.pending[1]["state"])
}

// TestResync_ReEnqueuesLastWhenPendingEmpty pins the other half: an EMPTY
// queue means there is genuinely nothing in flight, so Resync's original job
// (catch a reconnected/new document up to the current intent) still applies.
func TestResync_ReEnqueuesLastWhenPendingEmpty(t *testing.T) {
	fake := newFakeCDP()
	svc := newTestService(t, fake, validContract)

	svc.ShowReady()
	fake.waitForCalls(t, 1) // queue drains empty

	svc.Resync()
	fake.waitForCalls(t, 2)
	assert.Equal(t, stateReady, fake.lastRequest()["state"])
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
// contract: a manifest listing only the 7 required states (predating
// factory_reset, which the shipping manifest lists today) still passes the
// gate, and ShowFactoryReset is delivered to CDP anyway. Requiring
// factory_reset in the gate would instead disable ALL narration on any
// fielded player whose manifest predates it, so this guards against that
// regression.
func TestFactoryResetIsSafeAgainstCurrentManifest(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract) // validContract models a manifest predating the factory_reset extension state

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

// TestValidatePlayerStatusContract pins the sibling exported validator (design
// doc §4.3 / §8 housekeeping): same unreadable-vs-absent distinction as
// validateSetupDisplayContract, exposed for devicectl's capability fuse.
func TestValidatePlayerStatusContract(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		body := `{"contracts":{"playerStatus":{"version":1}}}`
		require.NoError(t, ValidatePlayerStatusContract(writeContract(t, body)))
	})
	t.Run("missing playerStatus", func(t *testing.T) {
		err := ValidatePlayerStatusContract(writeContract(t, contractWithoutSetupDisplay))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing contracts.playerStatus")
		assert.NotErrorIs(t, err, ErrPlayerContractUnreadable, "a READ manifest lacking the contract is absent, not unreadable")
	})
	t.Run("wrong version", func(t *testing.T) {
		body := `{"contracts":{"playerStatus":{"version":2}}}`
		err := ValidatePlayerStatusContract(writeContract(t, body))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version must be 1")
	})
	t.Run("unreadable manifest", func(t *testing.T) {
		err := ValidatePlayerStatusContract(filepath.Join(t.TempDir(), "does-not-exist.json"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPlayerContractUnreadable)
	})
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

// TestNavigationSetupUIIntegration_SkipsWhenOverlayOwnsScreen is half of the
// setupui↔playersession integration coverage the deleted RequestPageReload
// atomicity tests provided (design doc §5): with setupui registered as a
// REAL playersession.Session overlay owner, a navigation attempted while
// narration is up must be pre-nav-skipped — the narration racing the
// navigation's own gate check is exactly the case a caller-side
// probe-then-navigate pattern could lose.
func TestNavigationSetupUIIntegration_SkipsWhenOverlayOwnsScreen(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	session := playersession.New(context.Background(), sender, nil, nil, zap.NewNop())
	svc.SetSession(session)
	session.RegisterOverlayOwner("setupui", svc.Narrating)

	svc.ShowUpdating(10)
	sender.waitForCalls(t, 1)

	err := session.NavigateHomeInline(playersession.NavOptions{})
	assert.ErrorIs(t, err, playersession.ErrNavSkippedOverlay)
	assert.NotContains(t, sender.sentMethods(), "Page.navigate",
		"the overlay gate must stop the navigation before it ever sends")
}

// TestNavigationSetupUIIntegration_RacingNarrationDeliveredPostNavigation is
// the other half: with no overlay up, a navigation is allowed to run, and a
// narration push that races in WHILE it is executing (NavigationPending)
// must park (setupui.SetSession) and be delivered only once the navigation's
// own generation-ready worker confirms the replacement document's handler is
// installed — never lost by evaluating into the dying pre-navigation
// document. This is exactly the atomicity the old single-worker
// RequestPageReload lane provided, now reproduced across the two real
// components instead of asserted against one.
func TestNavigationSetupUIIntegration_RacingNarrationDeliveredPostNavigation(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	svc.navigationParkPollInterval = 5 * time.Millisecond
	svc.navigationParkTimeout = 2 * time.Second

	session := playersession.New(context.Background(), sender, nil, nil, zap.NewNop())
	svc.SetSession(session)
	session.RegisterOverlayOwner("setupui", svc.Narrating)

	// [M2] Establish a REAL, already-ready OLD generation before triggering
	// the navigation below, so the pre-bump window is genuine: without this,
	// session.Generation() starts at 0 (no generation ever established), and
	// the pre-fix parkForNavigation (StageReady alone, no generation check)
	// would accidentally behave correctly for the wrong reason — there is no
	// stale-ready generation for a false positive to come from, so this test
	// would pass whether or not the M2 bug was present.
	session.OnConnect()
	require.Eventually(t, func() bool { return session.StageReady(playersession.StageHandler) }, time.Second, time.Millisecond)

	sender.readyAfterProbes = 3 // the replacement document answers on the 4th probe

	navDone := make(chan playersession.NavResult, 1)
	session.NavigateHome(playersession.NavOptions{}, func(res playersession.NavResult) {
		navDone <- res
	})

	// Race a narration push in while the navigation is executing. Wait for
	// Page.navigate to have actually been SENT (not just NavigationPending —
	// which flips true at the very top of navigateAndVerify, before its own
	// overlay gate check) so ShowUpdating's synchronous s.last write cannot
	// race that gate check itself: this test targets the park mechanism
	// during the post-navigate transition window, not the overlay gate
	// (TestNavigationSetupUIIntegration_SkipsWhenOverlayOwnsScreen covers
	// that separately).
	require.Eventually(t, func() bool {
		for _, m := range sender.sentMethods() {
			if m == "Page.navigate" {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
	svc.ShowUpdating(10)

	select {
	case res := <-navDone:
		assert.Equal(t, playersession.NavExecuted, res.Outcome)
		assert.NoError(t, res.Err)
	case <-time.After(2 * time.Second):
		t.Fatal("navigation never completed")
	}

	// The racing push must still reach the screen — delivered into the
	// REPLACEMENT document, never lost into the dying one.
	deadline := time.After(2 * time.Second)
	for {
		if last := sender.lastRequest(); last != nil && last["state"] == "updating" {
			break
		}
		select {
		case <-sender.signal:
		case <-deadline:
			t.Fatalf("updating overlay never reached the replacement document; lostEvaluates=%d", sender.lostEvaluates)
		}
	}
	sender.mu.Lock()
	lost := sender.lostEvaluates
	sender.mu.Unlock()
	assert.Equal(t, 0, lost, "no narration may evaluate into the dying document")
}

// TestSweepStaleOverlay pins the boot-reconciliation sweep's atomicity with
// live narration: it hides only when THIS process has no narration intent
// yet, and both race directions with a concurrent narrator preserve the
// narration (the reviewer's cross-flow hazard: the claim flow's sweep and
// the startup OTA gate run as unordered goroutines on the same transition).
func TestSweepStaleOverlay(t *testing.T) {
	t.Run("no intent yet: sweeps the previous life's overlay", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.SweepStaleOverlay()
		sender.waitForCalls(t, 1)
		assert.Equal(t, stateHidden, sender.lastRequest()["state"])
	})

	t.Run("narration first, sweep delayed: narration stays authoritative", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.ShowUpdating(40)
		sender.waitForCalls(t, 1)
		svc.SweepStaleOverlay() // maximally delayed sweep: must no-op

		assert.True(t, svc.Narrating(), "sweep must not clear live narration intent")
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.Equal(t, 1, sender.callCount(), "the sweep must not deliver a hide")
	})

	t.Run("sweep first, narration racing in behind: screen ends on narration", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.SweepStaleOverlay()
		svc.ShowUpdating(10) // queued behind the hide on the same lane
		sender.waitForCalls(t, 2)
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.True(t, svc.Narrating())
	})
}

// TestHideIfShowing pins the owned-narration clear: a flow may hide the
// overlay it painted (finalizing) but must never erase a concurrent
// narrator's (updating), and with no intent at all it must not push a
// spurious hide.
func TestHideIfShowing(t *testing.T) {
	t.Run("hides its own narration", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.ShowFinalizing()
		svc.HideIfShowing(StateFinalizing)
		sender.waitForCalls(t, 2)
		assert.Equal(t, stateHidden, sender.lastRequest()["state"])
		assert.False(t, svc.Narrating())
	})

	t.Run("yields to a concurrent narrator", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.ShowFinalizing()
		svc.ShowUpdating(20) // another narrator took the screen mid-flow
		svc.HideIfShowing(StateFinalizing)
		sender.waitForCalls(t, 2)
		assert.True(t, svc.Narrating(), "updating must survive the finalizing owner's clear")
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.Equal(t, 2, sender.callCount(), "no hide may be delivered")
	})

	t.Run("no intent: no spurious hide", func(t *testing.T) {
		sender := newFakeCDP()
		svc := newTestService(t, sender, validContract)

		svc.HideIfShowing(StateFinalizing)
		assert.Equal(t, 0, sender.callCount())
		assert.False(t, svc.Narrating())
	})
}

// fakeNavigationSession is a minimal, directly-controllable NavigationSession
// double: tests flip pending/ready/generation to drive the worker's park loop
// without a real playersession.Session.
type fakeNavigationSession struct {
	mu      sync.Mutex
	pending bool
	ready   bool
	gen     uint64
	target  uint64
}

func (f *fakeNavigationSession) NavigationPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending
}

func (f *fakeNavigationSession) StageReady(playersession.Stage) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ready
}

func (f *fakeNavigationSession) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

func (f *fakeNavigationSession) NavigationTargetGeneration() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target
}

func (f *fakeNavigationSession) setPending(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = v
}

func (f *fakeNavigationSession) setReady(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready = v
}

func (f *fakeNavigationSession) setGeneration(v uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gen = v
}

func (f *fakeNavigationSession) setTarget(v uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.target = v
}

// TestParkForNavigation_NoSessionWired guards that every pre-session build
// (and every OTHER test in this file, none of which call SetSession) sees the
// worker never park: nil session must be indistinguishable from "no pending
// navigation" behavior-wise.
func TestParkForNavigation_NoSessionWired(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)

	start := time.Now()
	svc.ShowReady()
	sender.waitForCalls(t, 1)
	assert.Less(t, time.Since(start), time.Second)
}

// TestParkForNavigation_DeliversAfterNavigationPendingClears pins the primary
// exit: a push queued while a navigation is pending waits, then delivers the
// instant NavigationPending flips false — well before the park timeout.
func TestParkForNavigation_DeliversAfterNavigationPendingClears(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	nav := &fakeNavigationSession{pending: true}
	svc.SetSession(nav)
	svc.navigationParkPollInterval = 5 * time.Millisecond
	svc.navigationParkTimeout = 2 * time.Second

	svc.ShowReady()
	// Give the worker a chance to observe the pending flag and start parking.
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 0, sender.callCount(), "must park while NavigationPending is true")

	nav.setPending(false)
	sender.waitForCalls(t, 1)
	assert.Equal(t, stateReady, sender.lastRequest()["state"])
}

// TestParkForNavigation_ExitsWhenStageReady pins the second exit: the
// navigation's TARGET generation reaching StageHandler ends the park even
// while NavigationPending is still (momentarily) true. target must be
// non-zero AND match Generation() for StageReady to count [Park predicate
// fix] — setting ready alone, with target still 0 (pre-bump), must NOT exit
// (see the sibling pre-bump test below).
func TestParkForNavigation_ExitsWhenStageReady(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	nav := &fakeNavigationSession{pending: true}
	svc.SetSession(nav)
	svc.navigationParkPollInterval = 5 * time.Millisecond
	svc.navigationParkTimeout = 2 * time.Second

	svc.ShowReady()
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, 0, sender.callCount())

	// pending stays true; the target generation reaching StageReady must
	// still end the park.
	nav.setGeneration(1)
	nav.setTarget(1)
	nav.setReady(true)
	sender.waitForCalls(t, 1)
	assert.Equal(t, stateReady, sender.lastRequest()["state"])
}

// TestParkForNavigation_PreBumpStageReady_DoesNotExit pins the pre-bump half
// of the park predicate fix directly: StageReady answering against a stale,
// already-ready OLD generation while the navigation hasn't bumped one yet
// (target == 0) must never end the park. Before the ORIGINAL M2 fix, a bare
// StageReady(StageHandler) check made this a production no-op: the park
// exited immediately against the stale positive, delivering narration into a
// document about to be torn down.
func TestParkForNavigation_PreBumpStageReady_DoesNotExit(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	nav := &fakeNavigationSession{pending: true, ready: true} // gen 0, target 0 (pre-bump), already "ready"
	svc.SetSession(nav)
	svc.navigationParkPollInterval = 5 * time.Millisecond
	svc.navigationParkTimeout = 60 * time.Millisecond

	start := time.Now()
	svc.ShowReady()
	sender.waitForCalls(t, 1)
	elapsed := time.Since(start)

	// It must have taken (roughly) the full park timeout, not exited early
	// against the stale, pre-bump StageReady.
	assert.GreaterOrEqual(t, elapsed, 55*time.Millisecond,
		"a pre-bump (target==0) StageReady must not exit the park early")
	assert.Equal(t, stateReady, sender.lastRequest()["state"])
}

// TestParkForNavigation_ExitsPromptlyWhenEnteredPostBump pins finding 1
// directly: a park entered AFTER the navigation's bump already happened
// (Generation() and the navigation's target generation are already the
// SAME value when this park starts — a real, common timing since
// NavigationPending stays true for the navigation's entire verifyCap window
// while awaitRouteSettled keeps polling) must exit PROMPTLY once that
// target generation is handler-ready, not stall for the full park timeout.
// A Generation()-snapshot-at-entry comparison (the pre-fix approach) cannot
// tell this case apart from "no bump happened yet" — the snapshot IS
// already the new generation — so it stalled here for the full timeout.
func TestParkForNavigation_ExitsPromptlyWhenEnteredPostBump(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	nav := &fakeNavigationSession{pending: true, gen: 1, target: 1, ready: true}
	svc.SetSession(nav)
	svc.navigationParkPollInterval = 5 * time.Millisecond
	svc.navigationParkTimeout = 2 * time.Second

	start := time.Now()
	svc.ShowReady()
	sender.waitForCalls(t, 1)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 200*time.Millisecond,
		"a park entered post-bump must exit promptly, not stall for the full timeout")
	assert.Equal(t, stateReady, sender.lastRequest()["state"])
}

// TestParkForNavigation_TimesOutAndDeliversBestEffort pins the third exit: a
// navigation that never resolves (page never installs its handler) must not
// park narration for the process.
func TestParkForNavigation_TimesOutAndDeliversBestEffort(t *testing.T) {
	sender := newFakeCDP()
	svc := newTestService(t, sender, validContract)
	nav := &fakeNavigationSession{pending: true}
	svc.SetSession(nav)
	svc.navigationParkPollInterval = 5 * time.Millisecond
	svc.navigationParkTimeout = 30 * time.Millisecond

	svc.ShowReady()
	sender.waitForCalls(t, 1)
	assert.Equal(t, stateReady, sender.lastRequest()["state"])
}
