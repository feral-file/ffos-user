package provisioning

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
)

func TestQueuedCredentialRetryMasksRetainedFailure(t *testing.T) {
	h := newHarness(t)
	h.conn.online = true // initial assessment keeps joins outside AP-active
	failure := portal.Status{State: portal.JoinFailed, SSID: "HomeNet", Reason: "auth-failure"}
	h.m.status = failure
	// Leave the loop stopped to pin the admission-to-processing window.
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet", Password: "corrected"}))
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet", Password: "corrected-again"}))
	assert.Equal(t, portal.Status{State: portal.JoinInProgress}, h.m.Status())
	assert.Equal(t, failure, h.m.status, "retain the actual outcome internally")

	// A late/ignored submission must release its pending count too. The
	// harness is not AP-active, so the loop ignores both queued joins.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.m.loop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("provisioning loop did not stop")
		}
	})
	require.Eventually(t, func() bool { return h.m.Status() == failure }, time.Second, time.Millisecond)
	h.m.mu.Lock()
	assert.Zero(t, h.m.pendingJoins, "each processed admission must be released exactly once")
	h.m.mu.Unlock()
	// A successive retry must mask the outcome even after earlier joins finish.
	cancel()
	<-done
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	assert.Equal(t, portal.JoinInProgress, h.m.Status().State)
}

func TestStoppedSupervisorClearsQueuedCredentialRetries(t *testing.T) {
	h := newHarness(t)
	h.conn.online = true
	failure := portal.Status{State: portal.JoinFailed, Reason: "auth-failure"}
	h.m.status = failure
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	assert.Equal(t, portal.JoinInProgress, h.m.Status().State)

	// An already canceled run never consumes the queued joins. This also
	// exercises the cleanup used when cancellation ends panic backoff.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.m.Start(ctx)
	select {
	case <-h.m.done:
	case <-time.After(time.Second):
		t.Fatal("canceled supervisor did not stop")
	}
	assert.Equal(t, failure, h.m.Status())
	assert.Empty(t, h.m.events, "ended-run submissions must not replay on restart")
	assert.Zero(t, h.m.pendingJoins)
	h.m.Stop()

	// Ended runs reject new admissions rather than silently draining them.
	require.Error(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	assert.Equal(t, failure, h.m.Status())
	// Starting a fresh run reopens admission; ignored joins release their count.
	h.m.Start(context.Background())
	t.Cleanup(h.m.Stop)
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	require.Eventually(t, func() bool { return h.m.Status() == failure }, time.Second, time.Millisecond)
}

func TestStartProcessesPreStartCredentialRetries(t *testing.T) {
	h := newHarness(t)
	h.conn.online = true // queued joins are ignored outside AP-active
	failure := portal.Status{State: portal.JoinFailed, Reason: "auth-failure"}
	h.m.status = failure
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	assert.Equal(t, portal.JoinInProgress, h.m.Status().State)

	// Start installs a live admission context without discarding the queue.
	// The real supervisor must consume both pre-start events and release them.
	h.m.Start(context.Background())
	t.Cleanup(h.m.Stop)
	require.Eventually(t, func() bool { return h.m.Status() == failure }, time.Second, time.Millisecond)
	h.m.mu.Lock()
	assert.Zero(t, h.m.pendingJoins)
	h.m.mu.Unlock()
	h.m.Stop()
	assert.Empty(t, h.m.events)
	require.Error(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))

	// Stop waits for supervisor cleanup before Start can reopen admission.
	h.m.Start(context.Background())
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	require.Eventually(t, func() bool { return h.m.Status() == failure }, time.Second, time.Millisecond)
	h.m.mu.Lock()
	assert.Zero(t, h.m.pendingJoins)
	h.m.mu.Unlock()
}

func TestCanceledRunRejectsAdmissionBeforeCleanup(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	h.m.joinRunCtx = ctx
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	cancel()
	// Pin the supervisor-exit window before cleanup gets the state lock.
	// A successful admission here would otherwise be silently drained.
	require.Error(t, h.m.RequestJoin(portal.JoinRequest{SSID: "NewNet"}))
	assert.Equal(t, 1, h.m.pendingJoins)
	h.m.clearStoppedEvents()
	assert.Zero(t, h.m.pendingJoins)
	assert.Empty(t, h.m.events)
	require.Error(t, h.m.RequestJoin(portal.JoinRequest{SSID: "NewNet"}))
}

func TestRejectedCredentialRetryKeepsFailure(t *testing.T) {
	h := newHarness(t)
	failure := portal.Status{State: portal.JoinFailed, Reason: "auth-failure"}
	h.m.status = failure
	require.Error(t, h.m.RequestJoin(portal.JoinRequest{SSID: " "}))
	assert.Equal(t, failure, h.m.Status())
	for len(h.m.events) < cap(h.m.events) {
		h.m.events <- event{kind: evRescan}
	}
	require.Error(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet"}))
	assert.Equal(t, failure, h.m.Status(), "a rejected enqueue must not mask the failure indefinitely")
}

func TestQueuedCredentialRetryPublishesNewOutcome(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	h.m.state = StateAPActive
	h.m.status = portal.Status{State: portal.JoinFailed, SSID: "HomeNet", Reason: "auth-failure"}
	require.NoError(t, h.m.RequestJoin(portal.JoinRequest{SSID: "HomeNet", Password: "corrected"}))
	assert.Equal(t, portal.JoinInProgress, h.m.Status().State)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.m.loop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("provisioning loop did not stop")
		}
	})
	require.Eventually(t, func() bool { return h.m.Status().State == portal.JoinSucceeded }, time.Second, time.Millisecond)
	assert.Empty(t, h.m.Status().Reason, "the previous failure must not leak into the retry outcome")
}
