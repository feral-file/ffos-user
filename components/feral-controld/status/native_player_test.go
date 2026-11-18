package status

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

func nativeSnapshot() playerlink.Snapshot {
	return playerlink.Snapshot{Instance: "host-a", Generation: 2, Revision: 20, Host: "responsive", Renderer: "loading", Loop: "playlist", Context: "personal",
		Playlist: json.RawMessage(`{"id":"incoming","items":[{"source":"https://example.org/same"}]}`), PlaylistURL: "https://example.org/feed",
		PresentedPlaylist: json.RawMessage(`{"id":"outgoing","items":[{"source":"https://example.org/same"}]}`), PresentedPlaylistURL: "https://example.org/feed",
		Settings: playerlink.Settings{Framing: "artwork", Tombstone: "timed"},
		Selected: &playerlink.Occurrence{ID: "second", Generation: 2, Stage: "loading", Context: "personal", Item: json.RawMessage(`{"source":"https://example.org/same"}`)},
		Presented: &playerlink.Occurrence{ID: "first", Generation: 1, Stage: "presented", NavigationCommitted: true, Context: "curated", Item: json.RawMessage(`{"source":"https://example.org/same"}`),
			Display: playerlink.Display{Scaling: "fit", Margin: json.RawMessage(`"2%"`), Background: "#ffffff"}}}
}

func TestNativeStatusReportsPresentedPlaylistDuringReplacement(t *testing.T) {
	s := nativeSnapshot()
	got, err := NativePlayerStatus(s)
	require.NoError(t, err)
	require.Equal(t, "outgoing", got.Playlist.ID)
	require.Equal(t, "curated", got.ContentContext)
	require.Equal(t, 2, *got.RenderStatus)
	require.Equal(t, "first", *got.DeviceSettings.ShowingKey)
	require.Equal(t, "second", got.Runtime.Selected.ID)
	require.Equal(t, uint64(1), got.Runtime.Presented.Generation)
	require.Equal(t, uint64(2), got.Runtime.Generation)
	require.Nil(t, got.SignatureStatus)
	// The incoming renderer's failure remains observable without attributing
	// either its metadata or its failure to the retained outgoing occurrence.
	s.Selected.Stage = "failed"
	s.Selected.Error = "renderer terminated"
	s.Renderer = "failed"
	got, err = NativePlayerStatus(s)
	require.NoError(t, err)
	require.Equal(t, "outgoing", got.Playlist.ID)
	require.Equal(t, "renderer terminated", got.Runtime.Selected.Error)
	require.Equal(t, 2, *got.RenderStatus)
}

func TestNativeStatusNeverPromotesNavigationToReady(t *testing.T) {
	s := nativeSnapshot()
	s.Presented = nil
	s.PresentedPlaylist = nil
	for _, stage := range []string{"loading", "committed", "failed"} {
		s.Selected.Stage = stage
		got, err := NativePlayerStatus(s)
		require.NoError(t, err)
		require.NotEqual(t, 2, *got.RenderStatus)
		require.Nil(t, got.DeviceSettings.ShowingKey)
	}
	s.Selected = nil
	s.Stopped = true
	got, err := NativePlayerStatus(s)
	require.NoError(t, err)
	require.Nil(t, got.RenderStatus)
	require.Nil(t, got.Index)
}

func TestNativeStatusRetainsRecoveryFailureWhileReplacementLoads(t *testing.T) {
	s := nativeSnapshot()
	s.Recovery = &playerlink.RecoveryStatus{Attempt: 1, Limit: 3, LastError: "Decoder terminated", FailedOccurrence: "failed-attempt"}
	got, err := NativePlayerStatus(s)
	require.NoError(t, err)
	require.Equal(t, "outgoing", got.Playlist.ID)
	require.Equal(t, "Decoder terminated", got.Runtime.Recovery.LastError)
	require.Equal(t, "failed-attempt", got.Runtime.Recovery.FailedOccurrence)
	require.Equal(t, "loading", got.Runtime.Selected.Stage)
}

func TestNativeVerificationRequiresPresentedGeneration(t *testing.T) {
	got, err := NativePlayerStatus(nativeSnapshot())
	require.NoError(t, err)
	p := &poller{verificationLookup: func(string, string) (string, bool) { t.Fatal("unfenced legacy lookup used"); return "", false }}
	spoofed := "valid"
	got.SignatureStatus = &spoofed
	p.annotateSignatureStatus(got)
	require.Nil(t, got.SignatureStatus)
	p.nativeVerificationLookup = func(instance string, generation uint64, id, url string) (string, bool) {
		require.Equal(t, "host-a", instance)
		require.Equal(t, uint64(1), generation)
		require.Equal(t, "outgoing", id)
		require.Equal(t, "https://example.org/feed", url)
		return "valid", true
	}
	p.annotateSignatureStatus(got)
	require.Equal(t, "valid", *got.SignatureStatus)
}

func TestNativeStatusRejectsContradictoryIdentity(t *testing.T) {
	s := nativeSnapshot()
	s.Selected.Generation = s.Generation + 1
	_, err := NativePlayerStatus(s)
	require.Error(t, err)
	s = nativeSnapshot()
	s.Presented.NavigationCommitted = false
	_, err = NativePlayerStatus(s)
	require.Error(t, err)
}

func TestNativeStatusKeepsDeferredDocumentGeneration(t *testing.T) {
	s := nativeSnapshot()
	s.Selected.Generation = 1
	s.PendingUpdate = &playerlink.PendingUpdate{ID: "refresh", ItemCount: 3}
	s.UpdateOutcome = &playerlink.UpdateOutcome{ID: "refresh", Outcome: "deferred"}
	got, err := NativePlayerStatus(s)
	require.NoError(t, err)
	require.Equal(t, uint64(1), got.Runtime.Selected.Generation)
	require.Equal(t, s.Generation, got.Runtime.Generation)
	require.Equal(t, s.PendingUpdate, got.Runtime.PendingUpdate)
	require.Equal(t, s.UpdateOutcome, got.Runtime.UpdateOutcome)
}

func TestNativeStatusMetadataMismatchKeepsHostOutcomeObservable(t *testing.T) {
	s := nativeSnapshot()
	s.PresentedPlaylist = json.RawMessage(`{"items":[{"id":123,"source":"https://example.org/art"}]}`)
	got, err := NativePlayerStatus(s)
	require.NoError(t, err)
	require.False(t, got.Ok)
	require.Nil(t, got.Playlist)
	require.Nil(t, got.Items)
	require.NotEmpty(t, got.Runtime.MetadataError)
	require.Equal(t, "responsive", got.Runtime.Host)
	require.Equal(t, "first", got.Runtime.Presented.ID)
}

func TestPollNativeUnavailableReportsErrorAndStopsPlaybackAccounting(t *testing.T) {
	f := &fakePlayer{err: errors.New("native socket unavailable")}
	ws := &fakeWS{}
	p := &poller{player: f, ws: ws, relayer: &fakeRelayer{}, logger: zap.NewNop(),
		lastRelayerStatusHashes: map[relayer.NotificationType]string{}, lastWSStatusHashes: map[relayer.NotificationType]string{}}
	p.pollPlayerStatus(context.Background())
	require.Equal(t, 1, f.calls)
	require.False(t, p.lastIsPlaying)
	require.Equal(t, 1, ws.sendAllCalls)
	p.pollPlayerStatus(context.Background())
	require.Equal(t, 1, ws.sendAllCalls, "same outage notification is deduplicated")
}

func TestNativeStatusRetainsComposedSettingsWhilePreviewIsPending(t *testing.T) {
	s := nativeSnapshot()
	s.Presented.DisplayRevision = 3
	s.Presented.Display.Scaling = "fill"
	s.Presented.Display.Margin = json.RawMessage(`20`)
	s.Composition = &playerlink.Composition{Occurrence: s.Presented.ID, Revision: 8, DisplayRevision: 2,
		Display: playerlink.Display{Scaling: "fit", Margin: json.RawMessage(`"10%"`), Background: "#123456"}}
	got, err := NativePlayerStatus(s)
	require.NoError(t, err)
	require.Equal(t, "fit", *got.DeviceSettings.Scaling)
	require.JSONEq(t, `"10%"`, string(got.DeviceSettings.Margin))
	require.Equal(t, uint64(8), *got.DeviceSettings.CompositionRevision)
	s.Composition.Occurrence = "retired"
	_, err = NativePlayerStatus(s)
	require.ErrorContains(t, err, "composition identity")
}
