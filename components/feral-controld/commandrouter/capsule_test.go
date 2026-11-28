package commandrouter_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type capsuleImporter struct {
	offlinecache.Service
	importArchive func([]byte, json.RawMessage, string) (json.RawMessage, error)
}

func (c capsuleImporter) ImportCapsule(_ context.Context, b []byte, s json.RawMessage, k string) (json.RawMessage, error) {
	return c.importArchive(b, s, k)
}

type capsulePlayer struct{ sent []commands.Command }

func (p *capsulePlayer) Ready(context.Context) bool { return true }
func (p *capsulePlayer) Send(_ context.Context, cmd commands.Command) (any, error) {
	p.sent = append(p.sent, cmd)
	return playerOkResponse(), nil
}

func TestOwnerCapsuleUsesPlaylistRouter(t *testing.T) {
	ts := setup(t)
	archive := []byte("signed fixture")
	const signer = "publisher"
	const raw = `{"dpVersion":"1.1.0","title":"Edition","items":[{"source":"https://example.org/work","license":"open"}]}`
	p := &capsulePlayer{}
	importer := capsuleImporter{importArchive: func(b []byte, s json.RawMessage, k string) (json.RawMessage, error) {
		require.Equal(t, archive, b)
		require.JSONEq(t, `{"id":"fixture"}`, string(s))
		require.Equal(t, signer, k)
		return json.RawMessage(raw), nil
	}}
	h := commandrouter.New(ts.mockExecutor, p, ts.mockDP1, ts.mockStatusPoller, nil, importer, nil, nil, wrapper.NewJSON(), ts.logger)
	ctx := control.WithPrincipal(t.Context(), control.Principal{Role: control.RoleOwner, ControllerID: "owner"})
	args := map[string]any{"capsule": base64.StdEncoding.EncodeToString(archive), "signer": signer, "signature": map[string]string{"id": "fixture"}}
	ts.mockDP1.EXPECT().ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), dp1.CAST_PLAYLIST_ITEMS_LIMIT).DoAndReturn(passthroughCastPlan)
	ts.mockStatusPoller.EXPECT().ForceRefresh()
	_, err := h.Process(ctx, commands.Command{Type: commands.CMD_DISPLAY_CAPSULE, Arguments: args})
	require.NoError(t, err)
	require.Len(t, p.sent, 1)
	require.Equal(t, commands.CMD_DISPLAY_PLAYLIST, p.sent[0].Type)
	hash := sha256.Sum256(archive)
	require.Equal(t, hex.EncodeToString(hash[:]), p.sent[0].OfflineCapsuleID)
}

func TestRejectedCapsuleNeverReachesPlayer(t *testing.T) {
	ts := setup(t)
	p := &capsulePlayer{}
	ctx := control.WithPrincipal(t.Context(), control.Principal{Role: control.RoleOwner, ControllerID: "owner"})
	okArgs := map[string]any{"capsule": "YQ==", "signer": "publisher"}
	importer := capsuleImporter{importArchive: func([]byte, json.RawMessage, string) (json.RawMessage, error) {
		return nil, errors.New("archive not admitted")
	}}
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		cache  offlinecache.Service
		args   map[string]any
		reason string
	}{
		{"controller", control.WithPrincipal(t.Context(), control.Principal{Role: control.RoleController, ControllerID: "controller"}), importer, okArgs, commandrouter.ErrPermissionDenied.Error()},
		{"no owner", t.Context(), importer, okArgs, commandrouter.ErrPermissionDenied.Error()},
		{"no cache", ctx, nil, okArgs, "offline cache"},
		{"missing signer", ctx, importer, map[string]any{"capsule": "YQ=="}, "requires"},
		{"invalid encoding", ctx, importer, map[string]any{"capsule": "!", "signer": "publisher"}, "base64"},
		{"invalid sidecar", ctx, importer, map[string]any{"capsule": "YQ==", "signer": "publisher", "signature": make(chan int)}, "signature document"},
		{"unverified archive", ctx, importer, okArgs, "archive not admitted"},
		{"unreadable playlist", ctx, capsuleImporter{importArchive: func([]byte, json.RawMessage, string) (json.RawMessage, error) { return json.RawMessage("null?"), nil }}, okArgs, "invalid character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := commandrouter.New(ts.mockExecutor, p, ts.mockDP1, ts.mockStatusPoller, nil, tc.cache, nil, nil, wrapper.NewJSON(), ts.logger)
			_, err := h.Process(tc.ctx, commands.Command{Type: commands.CMD_DISPLAY_CAPSULE, Arguments: tc.args})
			require.ErrorContains(t, err, tc.reason)
		})
	}
	require.Empty(t, p.sent)
}
