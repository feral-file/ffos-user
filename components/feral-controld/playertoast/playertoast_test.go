package playertoast_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	mockCDP.EXPECT().NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
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
	mockCDP.EXPECT().NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
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
		mockCDP.EXPECT().NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).Return(okResult(), nil).Times(1)
		s := playertoast.New(mockCDP, filepath.Join("..", "setupui", "testdata", "ffos-player-contract.json"), nil)
		assert.NoError(t, s.Show(context.Background(), notice), "mirror must list %q", notice)
	}
	_ = errors.New
}
