package status

import (
	"context"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// The player's outcome ring reaches the playback observer whole and is
// stripped from the notification; the notification instead carries the
// controld-owned per-item verdicts, index-aligned with items, and never a
// player-supplied value for that field.
func TestPollPlayerStatus_MediaHealthAnnotationAndOutcomeStripping(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURL: constants.WEBAPP_URL,
		noLogSendResult: map[string]interface{}{
			"message": map[string]interface{}{
				"ok":          true,
				"castCommand": "displayPlaylist",
				"index":       0,
				"items": []interface{}{
					map[string]interface{}{"id": "a", "source": "https://o/a"},
					map[string]interface{}{"id": "b", "source": "https://o/b"},
				},
				"outcomes": map[string]interface{}{
					"castSeq": 3,
					"entries": []interface{}{
						map[string]interface{}{"seq": 1, "source": "https://o/a", "kind": "fail", "stage": "load", "code": "image"},
					},
				},
				// A spoofed annotation from the page: must be discarded.
				"mediaHealth": []interface{}{map[string]interface{}{"state": "ok"}, map[string]interface{}{"state": "ok"}},
			},
		},
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}
	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}
	var observedOutcomes *mediahealth.Outcomes
	p.playbackObserver = func(st *PlayerStatus) {
		if st != nil {
			observedOutcomes = st.Outcomes
		}
	}
	var asked []dp1playlist.PlaylistItem
	p.itemHealthSource = func(items []dp1playlist.PlaylistItem) []mediahealth.ItemHealth {
		asked = items
		return []mediahealth.ItemHealth{
			{State: mediahealth.StateBroken, Category: mediahealth.CategoryRenderFailed, Reason: mediahealth.ReasonRenderLoad, Layer: mediahealth.LayerPlay},
			{State: mediahealth.StateUnknown},
		}
	}

	p.pollPlayerStatus(context.Background())

	require.NotNil(t, observedOutcomes, "the raw ring reaches the observer")
	assert.Equal(t, int64(3), observedOutcomes.CastSeq)
	require.Len(t, observedOutcomes.Entries, 1)
	assert.Equal(t, "https://o/a", observedOutcomes.Entries[0].Source)

	require.Len(t, asked, 2)
	assert.Equal(t, "https://o/b", asked[1].Source, "the source asks with the reply's own item list, sources intact")

	payload := mockWS.lastPayload.(map[string]interface{})
	message := payload["message"].(*PlayerStatus)
	assert.Nil(t, message.Outcomes, "the ring carries sources and is not a controller surface")
	require.Len(t, message.MediaHealth, 2)
	assert.Equal(t, mediahealth.StateBroken, message.MediaHealth[0].State)
	assert.Equal(t, mediahealth.CategoryRenderFailed, message.MediaHealth[0].Category)
	assert.Equal(t, mediahealth.StateUnknown, message.MediaHealth[1].State)
	require.Len(t, *message.Items, 2)
	assert.Empty(t, (*message.Items)[0].Source, "sources are still stripped")
}

func TestAnnotateMediaHealth_DropsMisalignedAndUnwired(t *testing.T) {
	items := []dp1playlist.PlaylistItem{{Source: "https://o/a"}, {Source: "https://o/b"}}
	spoofed := []mediahealth.ItemHealth{{State: mediahealth.StateOK}}

	p := &poller{}
	st := &PlayerStatus{Items: &items, MediaHealth: spoofed}
	p.annotateMediaHealth(st)
	assert.Nil(t, st.MediaHealth, "unwired: whatever the reply carried is dropped")

	p.itemHealthSource = func([]dp1playlist.PlaylistItem) []mediahealth.ItemHealth {
		return []mediahealth.ItemHealth{{State: mediahealth.StateOK}}
	}
	st = &PlayerStatus{Items: &items, MediaHealth: spoofed}
	p.annotateMediaHealth(st)
	assert.Nil(t, st.MediaHealth, "a list of another length would label the wrong work")

	st = &PlayerStatus{MediaHealth: spoofed}
	p.annotateMediaHealth(st)
	assert.Nil(t, st.MediaHealth, "no item list, no annotation")
}
