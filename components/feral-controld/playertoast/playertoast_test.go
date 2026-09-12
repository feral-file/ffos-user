package playertoast_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
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

func okResult() interface{} {
	return map[string]any{"message": map[string]any{"ok": true}}
}

// TestShow_SendsTheNoticeAndValidatesOK: a listed notice reaches the player as
// window.handleCDPRequest({command:"playerToast",request:{notice:...}}) and a
// {ok:true} reply is a success.
func TestShow_SendsTheNoticeAndValidatesOK(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	var expr string
	mockCDP.EXPECT().NoLogSendWithin(cdp.METHOD_EVALUATE, gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}, _ time.Duration) (interface{}, error) {
			expr, _ = params["expression"].(string)
			assert.Equal(t, true, params["returnByValue"])
			return okResult(), nil
		}).Times(1)

	s := playertoast.New(mockCDP, writeManifest(t, manifestWithToast), nil)
	require.NoError(t, s.Show(context.Background(), sigverify.NoticeInvalid))

	assert.True(t, strings.HasPrefix(expr, "window.handleCDPRequest("))
	payload := strings.TrimSuffix(strings.TrimPrefix(expr, "window.handleCDPRequest("), ")")
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &got))
	assert.Equal(t, "playerToast", got["command"])
	assert.Equal(t, map[string]any{"notice": "signature_invalid"}, got["request"])
}

// TestShow_PlayerRejection is a transport-OK reply with ok:false.
func TestShow_PlayerRejection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().NoLogSendWithin(cdp.METHOD_EVALUATE, gomock.Any(), gomock.Any()).
		Return(map[string]any{"message": map[string]any{"ok": false}}, nil).Times(1)

	s := playertoast.New(mockCDP, writeManifest(t, manifestWithToast), nil)
	err := s.Show(context.Background(), sigverify.NoticeUnsigned)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected")
}

// TestShow_UnsupportedWhenContractAbsent: an older player whose manifest
// decodes but omits playerToast yields ErrUnsupported and never sends.
func TestShow_UnsupportedWhenContractAbsent(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl) // any NoLogSend is an unexpected call

	s := playertoast.New(mockCDP, writeManifest(t, `{"contracts":{"setupDisplay":{"version":1}}}`), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid)
	assert.ErrorIs(t, err, playertoast.ErrUnsupported)
}

// TestShow_UnreadableManifestIsTransient: a missing/torn manifest is
// ErrContractUnreadable (re-checked next time), never ErrUnsupported, and
// nothing is sent.
func TestShow_UnreadableManifestIsTransient(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)

	s := playertoast.New(mockCDP, filepath.Join(t.TempDir(), "missing.json"), nil)
	err := s.Show(context.Background(), sigverify.NoticeInvalid)
	assert.ErrorIs(t, err, playertoast.ErrContractUnreadable)
	assert.NotErrorIs(t, err, playertoast.ErrUnsupported)

	// A torn (undecodable) write is unreadable too, not "unsupported".
	s2 := playertoast.New(mockCDP, writeManifest(t, `{"contracts":`), nil)
	assert.ErrorIs(t, s2.Show(context.Background(), sigverify.NoticeInvalid), playertoast.ErrContractUnreadable)
}

// TestShow_NoticeNotListed: the contract is present but does not list the
// notice — a distinct error from ErrUnsupported (the notice set is closed and
// should always be listed, so this guards a manifest/const drift).
func TestShow_NoticeNotListed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	body := `{"contracts":{"playerToast":{"version":1,"requestKey":"request","states":["signature_invalid"],"acceptedResponse":{"ok":true}}}}`

	s := playertoast.New(mockCDP, writeManifest(t, body), nil)
	err := s.Show(context.Background(), sigverify.NoticeRejected)
	require.Error(t, err)
	assert.NotErrorIs(t, err, playertoast.ErrUnsupported)
	assert.Contains(t, err.Error(), "not listed")
}

// TestShow_CanceledContext returns before touching the manifest or CDP.
func TestShow_CanceledContext(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCDP := mocks.NewMockCDP(ctrl)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := playertoast.New(mockCDP, writeManifest(t, manifestWithToast), nil)
	assert.ErrorIs(t, s.Show(ctx, sigverify.NoticeInvalid), context.Canceled)
}

// TestShow_ShippingMirrorListsEveryNotice pins the daemon's manifest mirror
// against the notice constants: every notice the policy can emit must be a
// state the contract lists, or a real device would reject the send.
func TestShow_ShippingMirrorListsEveryNotice(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	for _, notice := range []sigverify.Notice{sigverify.NoticeInvalid, sigverify.NoticeUnsigned, sigverify.NoticeRejected} {
		mockCDP := mocks.NewMockCDP(ctrl)
		mockCDP.EXPECT().NoLogSendWithin(cdp.METHOD_EVALUATE, gomock.Any(), gomock.Any()).Return(okResult(), nil).Times(1)
		s := playertoast.New(mockCDP, filepath.Join("..", "setupui", "testdata", "ffos-player-contract.json"), nil)
		assert.NoError(t, s.Show(context.Background(), notice), "mirror must list %q", notice)
	}
	_ = errors.New
}

// recordingSender is a Sender double for Dispatcher tests: it reports each
// notice it is asked to show and can block inside Show to model an in-flight
// send while later submissions arrive.
type recordingSender struct {
	shown   chan sigverify.Notice
	entered chan struct{}
	block   chan struct{}
}

func (r *recordingSender) Show(_ context.Context, n sigverify.Notice) error {
	if r.entered != nil {
		r.entered <- struct{}{}
	}
	if r.block != nil {
		<-r.block
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

	assert.Equal(t, sigverify.NoticeInvalid, awaitNotice(t, rec.shown), "the in-flight send completes")
	assert.Equal(t, sigverify.NoticeRejected, awaitNotice(t, rec.shown), "then only the latest queued is sent")
	// The superseded middle notice is never sent.
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

	assert.Equal(t, sigverify.NoticeInvalid, awaitNotice(t, rec.shown))
	select {
	case n := <-rec.shown:
		t.Fatalf("a cleared notice was sent: %q", n)
	case <-time.After(150 * time.Millisecond):
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
