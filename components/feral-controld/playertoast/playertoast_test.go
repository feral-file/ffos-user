package playertoast_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/playertoast"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

const manifestWithToast = `{"contracts":{"playerToast":{"version":1,"requestKey":"request","states":["signature_invalid","signature_unsigned","signature_rejected"],"acceptedResponse":{"ok":true}}}}`

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffos-player-contract.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// replyWith is the raw Runtime.evaluate result member a session hands back
// when window.handleCDPRequest returned {messageID, message:{ok}} by value.
func replyWith(ok bool) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"result":{"type":"object","value":{"messageID":"m1","message":{"ok":%t}}}}`, ok))
}

// fakeSession is the one-shot session a toast rides on: it records the
// evaluate it was asked to send, the deadline it was sent under, and whether
// it was closed afterwards.
type fakeSession struct {
	reply        json.RawMessage
	err          error
	sent         []map[string]interface{}
	sendDeadline time.Time
	closed       int
}

func (f *fakeSession) Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	if method != cdp.METHOD_EVALUATE {
		return nil, fmt.Errorf("unexpected method %s", method)
	}
	f.sendDeadline, _ = ctx.Deadline()
	f.sent = append(f.sent, params)
	return f.reply, f.err
}

func (f *fakeSession) Close() error {
	f.closed++
	return nil
}

// fakeDialer hands out one fakeSession (or fails the dial) and counts dials.
// onDial runs during the dial, modeling a transition that lands while the
// dial is in flight.
type fakeDialer struct {
	session *fakeSession
	err     error
	dials   int
	onDial  func()
}

func (d *fakeDialer) dial(context.Context) (playertoast.Session, error) {
	d.dials++
	if d.onDial != nil {
		d.onDial()
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.session, nil
}

// neverDial fails the test if the sender reaches for a session at all.
func neverDial(t *testing.T) playertoast.Dialer {
	return func(context.Context) (playertoast.Session, error) {
		t.Fatal("unexpected dial: the sender must decide before touching CDP")
		return nil, nil
	}
}

// TestShow_SendsTheNoticeAndValidatesOK: a listed notice reaches the player as
// window.handleCDPRequest({command:"playerToast",request:{notice:...}}) over
// a session dialed for it, a {ok:true} reply is a success, and the session
// is closed afterwards.
func TestShow_SendsTheNoticeAndValidatesOK(t *testing.T) {
	sess := &fakeSession{reply: replyWith(true)}
	d := &fakeDialer{session: sess}

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	require.NoError(t, s.Show(context.Background(), sigverify.NoticeInvalid, nil))

	assert.Equal(t, 1, d.dials)
	require.Len(t, sess.sent, 1)
	assert.Equal(t, true, sess.sent[0]["returnByValue"])
	expr, _ := sess.sent[0]["expression"].(string)
	assert.True(t, strings.HasPrefix(expr, "window.handleCDPRequest("))
	payload := strings.TrimSuffix(strings.TrimPrefix(expr, "window.handleCDPRequest("), ")")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &got))
	assert.Equal(t, "playerToast", got["command"])
	assert.Equal(t, map[string]any{"notice": "signature_invalid"}, got["request"])
	assert.Equal(t, 1, sess.closed, "the one-shot session is closed after the send")
	assert.False(t, sess.sendDeadline.IsZero(), "a caller without a deadline gets the default bound")
}

// TestShow_CallerDeadlineWins: a caller's own (tighter) deadline is the one
// the dial and evaluate run under, not the default.
func TestShow_CallerDeadlineWins(t *testing.T) {
	sess := &fakeSession{reply: replyWith(true)}
	d := &fakeDialer{session: sess}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	want, _ := ctx.Deadline()

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	require.NoError(t, s.Show(ctx, sigverify.NoticeInvalid, nil))
	assert.True(t, sess.sendDeadline.Equal(want), "evaluate ran under the caller's deadline")
}

// TestShow_PlayerRejection is a transport-OK reply with ok:false; the session
// is still closed.
func TestShow_PlayerRejection(t *testing.T) {
	sess := &fakeSession{reply: replyWith(false)}
	d := &fakeDialer{session: sess}

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	err := s.Show(context.Background(), sigverify.NoticeUnsigned, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected")
	assert.Equal(t, 1, sess.closed)
}

// TestShow_EvaluateExceptionIsAnError: a thrown handleCDPRequest surfaces as
// exceptionDetails in the raw result and is reported, not read as ok.
func TestShow_EvaluateExceptionIsAnError(t *testing.T) {
	sess := &fakeSession{reply: json.RawMessage(`{"result":{"type":"undefined"},"exceptionDetails":{"text":"Uncaught"}}`)}
	d := &fakeDialer{session: sess}

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exception")
	assert.Equal(t, 1, sess.closed)
}

// TestShow_DialFailureIsReported: no kiosk page to dial (headless, Chromium
// restarting) is an error the caller logs and drops — nothing else happens.
func TestShow_DialFailureIsReported(t *testing.T) {
	dialErr := errors.New("no page target found")
	d := &fakeDialer{err: dialErr}

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid, nil)
	assert.ErrorIs(t, err, dialErr)
	assert.Equal(t, 1, d.dials)
}

// TestShow_SendFailureStillClosesSession: a failed evaluate never leaks the
// session it was dialed on.
func TestShow_SendFailureStillClosesSession(t *testing.T) {
	sendErr := errors.New("cdp session closed")
	sess := &fakeSession{err: sendErr}
	d := &fakeDialer{session: sess}

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid, nil)
	assert.ErrorIs(t, err, sendErr)
	assert.Equal(t, 1, sess.closed)
}

// TestShow_UnsupportedWhenContractAbsent: an older player whose manifest
// decodes but omits playerToast yields ErrUnsupported and never dials.
func TestShow_UnsupportedWhenContractAbsent(t *testing.T) {
	s := playertoast.New(neverDial(t), writeManifest(t, `{"contracts":{"setupDisplay":{"version":1}}}`), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid, nil)
	assert.ErrorIs(t, err, playertoast.ErrUnsupported)
}

// TestShow_UnreadableManifestIsTransient: a missing/torn manifest is
// ErrContractUnreadable (re-checked next time), never ErrUnsupported, and
// nothing is dialed.
func TestShow_UnreadableManifestIsTransient(t *testing.T) {
	s := playertoast.New(neverDial(t), filepath.Join(t.TempDir(), "missing.json"), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid, nil)
	assert.ErrorIs(t, err, playertoast.ErrContractUnreadable)
	assert.NotErrorIs(t, err, playertoast.ErrUnsupported)

	// A torn (undecodable) write is unreadable too, not "unsupported".
	s2 := playertoast.New(neverDial(t), writeManifest(t, `{"contracts":`), nil)
	assert.ErrorIs(t, s2.Show(context.Background(), sigverify.NoticeInvalid, nil), playertoast.ErrContractUnreadable)
}

// TestShow_NoticeNotListed: the contract is present but does not list the
// notice — a distinct error from ErrUnsupported (the notice set is closed and
// should always be listed, so this guards a manifest/const drift).
func TestShow_NoticeNotListed(t *testing.T) {
	body := `{"contracts":{"playerToast":{"version":1,"requestKey":"request","states":["signature_invalid"],"acceptedResponse":{"ok":true}}}}`

	s := playertoast.New(neverDial(t), writeManifest(t, body), nil)
	err := s.Show(context.Background(), sigverify.NoticeRejected, nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, playertoast.ErrUnsupported)
	assert.Contains(t, err.Error(), "not listed")
}

// TestShow_CanceledContext returns before touching the manifest or dialing.
func TestShow_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := playertoast.New(neverDial(t), writeManifest(t, manifestWithToast), nil)
	assert.ErrorIs(t, s.Show(ctx, sigverify.NoticeInvalid, nil), context.Canceled)
}

// TestShow_ShippingMirrorListsEveryNotice pins the daemon's manifest mirror
// against the notice constants: every notice the policy can emit must be a
// state the contract lists, or a real device would reject the send.
func TestShow_ShippingMirrorListsEveryNotice(t *testing.T) {
	for _, notice := range []sigverify.Notice{sigverify.NoticeInvalid, sigverify.NoticeUnsigned, sigverify.NoticeRejected} {
		d := &fakeDialer{session: &fakeSession{reply: replyWith(true)}}
		s := playertoast.New(d.dial, filepath.Join("..", "setupui", "testdata", "ffos-player-contract.json"), nil)
		assert.NoError(t, s.Show(context.Background(), notice, nil), "mirror must list %q", notice)
		assert.Equal(t, 1, d.dials)
	}
}

// TestShow_AbandonsWhenNotCurrent: a notice superseded during manifest
// validation is abandoned before any dial.
func TestShow_AbandonsWhenNotCurrent(t *testing.T) {
	s := playertoast.New(neverDial(t), writeManifest(t, manifestWithToast), nil)
	assert.NoError(t, s.Show(context.Background(), sigverify.NoticeInvalid, func() bool { return false }))
}

// TestShow_AbandonsAfterDialWhenSuperseded: the dial is the longest step, so
// a transition that lands during it must still suppress the notice — the
// sender re-checks after dialing, sends nothing, and closes the session.
func TestShow_AbandonsAfterDialWhenSuperseded(t *testing.T) {
	sess := &fakeSession{reply: replyWith(true)}
	current := true
	d := &fakeDialer{session: sess, onDial: func() { current = false }}

	s := playertoast.New(d.dial, writeManifest(t, manifestWithToast), nil)
	require.NoError(t, s.Show(context.Background(), sigverify.NoticeInvalid, func() bool { return current }))
	assert.Equal(t, 1, d.dials)
	assert.Empty(t, sess.sent, "a superseded notice must not reach the player")
	assert.Equal(t, 1, sess.closed, "the session is still closed")
}

// recordingSender is a Sender double for Dispatcher tests: it reports each
// notice it is asked to show and can block inside Show to model an in-flight
// send while later submissions arrive.
type recordingSender struct {
	shown   chan sigverify.Notice
	entered chan struct{}
	block   chan struct{}
}

func (r *recordingSender) Show(_ context.Context, n sigverify.Notice, stillCurrent func() bool) error {
	if r.entered != nil {
		r.entered <- struct{}{}
	}
	if r.block != nil {
		<-r.block
	}
	// Model the real sender's CDP-handoff re-check: a Clear during the (here,
	// blocked) manifest-read window abandons the send.
	if stillCurrent != nil && !stillCurrent() {
		return nil
	}
	r.shown <- n
	return nil
}

func awaitNotice(t *testing.T, ch chan sigverify.Notice) sigverify.Notice {
	t.Helper()
	select {
	case n := <-ch:
		return n
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not send a notice")
		return ""
	}
}

// TestDispatcher_NotifySends: a submitted notice reaches the sender.
func TestDispatcher_NotifySends(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recordingSender{shown: make(chan sigverify.Notice, 4)}
	d := playertoast.NewDispatcher(ctx, rec, time.Second, nil)

	d.Notify(sigverify.NoticeUnsigned)
	assert.Equal(t, sigverify.NoticeUnsigned, awaitNotice(t, rec.shown))
}

// TestDispatcher_LatestWins: while one send is in flight, a burst of
// submissions collapses to the LAST one — a newer transition supersedes a
// queued older one (feral-file/ffos-user#307).
func TestDispatcher_LatestWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recordingSender{shown: make(chan sigverify.Notice, 4), entered: make(chan struct{}, 1), block: make(chan struct{})}
	d := playertoast.NewDispatcher(ctx, rec, time.Second, nil)

	d.Notify(sigverify.NoticeInvalid) // worker takes this and blocks in Show
	<-rec.entered                     // ensure the worker is IN Show(invalid)
	d.Notify(sigverify.NoticeUnsigned)
	d.Notify(sigverify.NoticeRejected) // the latest queued
	close(rec.block)                   // release the in-flight send

	// The in-flight Invalid was superseded while sending, so it is abandoned at
	// its CDP-handoff re-check; only the latest queued notice is delivered.
	assert.Equal(t, sigverify.NoticeRejected, awaitNotice(t, rec.shown), "only the latest is sent")
	select {
	case n := <-rec.shown:
		t.Fatalf("a superseded notice was sent: %q", n)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDispatcher_ClearDropsPending: Clear drops a not-yet-sent notice so a
// valid/silent transition's supersede keeps a stale warning off the wall.
func TestDispatcher_ClearDropsPending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recordingSender{shown: make(chan sigverify.Notice, 4), entered: make(chan struct{}, 1), block: make(chan struct{})}
	d := playertoast.NewDispatcher(ctx, rec, time.Second, nil)

	d.Notify(sigverify.NoticeInvalid)  // worker takes this, blocks in Show
	<-rec.entered                      // ensure the worker is IN Show(invalid)
	d.Notify(sigverify.NoticeUnsigned) // queued
	d.Clear()                          // drop the queued one
	close(rec.block)

	// The in-flight Invalid is superseded and abandoned at its handoff, and the
	// queued Unsigned was Cleared: nothing reaches the wall.
	select {
	case n := <-rec.shown:
		t.Fatalf("a superseded or cleared notice was sent: %q", n)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDispatcher_StopsWithContext: the worker exits when its context is done.
func TestDispatcher_StopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &recordingSender{shown: make(chan sigverify.Notice, 4)}
	d := playertoast.NewDispatcher(ctx, rec, time.Second, nil)
	cancel()
	// A submit after shutdown must not deadlock or panic; it simply may not send.
	d.Notify(sigverify.NoticeInvalid)
	select {
	case <-rec.shown:
	case <-time.After(150 * time.Millisecond):
	}
}

// TestDispatcher_NotifyIfEpoch_StaleIsNoop: a notice fenced to an epoch that a
// later Clear/Notify has advanced past is dropped (the F1 interleaving:
// a Clear between an epoch snapshot and the notify).
func TestDispatcher_NotifyIfEpoch_StaleIsNoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recordingSender{shown: make(chan sigverify.Notice, 4)}
	d := playertoast.NewDispatcher(ctx, rec, time.Second, nil)

	epoch := d.Epoch()
	d.Clear() // a newer transition advances the epoch
	d.NotifyIfEpoch(sigverify.NoticeInvalid, epoch)
	select {
	case n := <-rec.shown:
		t.Fatalf("a stale-epoch notice was sent: %q", n)
	case <-time.After(150 * time.Millisecond):
	}

	// Fenced to the CURRENT epoch, it is sent.
	d.NotifyIfEpoch(sigverify.NoticeUnsigned, d.Epoch())
	assert.Equal(t, sigverify.NoticeUnsigned, awaitNotice(t, rec.shown))
}

// TestDispatcher_ClearDuringSendAbandons: a Clear that lands while a dequeued
// notice is in Show (before the CDP handoff) abandons the send, so an obsolete
// warning never reaches the wall (F2).
func TestDispatcher_ClearDuringSendAbandons(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &recordingSender{shown: make(chan sigverify.Notice, 4), entered: make(chan struct{}, 1), block: make(chan struct{})}
	d := playertoast.NewDispatcher(ctx, rec, time.Second, nil)

	d.Notify(sigverify.NoticeInvalid) // worker dequeues and enters Show
	<-rec.entered
	d.Clear()        // a newer valid/silent transition supersedes it
	close(rec.block) // Show reaches its CDP-handoff re-check and abandons

	select {
	case n := <-rec.shown:
		t.Fatalf("an abandoned notice was sent: %q", n)
	case <-time.After(200 * time.Millisecond):
	}
}
