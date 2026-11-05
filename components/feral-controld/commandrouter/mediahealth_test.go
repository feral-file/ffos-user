package commandrouter_test

// Media health on the displayPlaylist path (feral-file/ffos-user#392): the
// per-item hint field rides every cast on the FINAL item list, a
// controller can never inject it, and getMediaHealth reports one verdict
// per item of the playlist on screen. The pass's verdicts and the admission
// rule are covered by handler_sourceprobe_test.go; the probe itself by the
// mediahealth package.

import (
	"errors"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

func TestDisplayPlaylist_CarriesHintsForTheFinalItemList(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead", "https://origin.example/alive"), nil).
		Times(1)
	var expr string
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ = params["expression"].(string)
			return playerOkResponse(), nil
		}).
		Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	fake := &fakeMediaHealth{
		results: []mediahealth.ProbeResult{
			{Source: "https://origin.example/dead", Verdict: mediahealth.VerdictDead, Status: 404, Reason: mediahealth.ReasonHTTPStatus},
			{Source: "https://origin.example/alive", Verdict: mediahealth.VerdictAlive, Status: 200},
		},
		hints: map[string]mediahealth.Hint{
			"https://origin.example/dead": {State: mediahealth.StateBroken, Category: mediahealth.CategorySourceMissing, Reason: mediahealth.ReasonHTTPStatus},
		},
	}
	commandrouter.SetMediaHealth(ts.handler, fake, ts.logger)

	// A controller-supplied hint map must be dropped, never forwarded.
	command := displayPlaylistURLCommand(playlistURL)
	command.Arguments["mediaHealth"] = map[string]interface{}{"https://origin.example/alive": map[string]interface{}{"state": "broken"}}

	_, err := ts.handler.Process(ts.ctx, command)
	require.NoError(t, err)

	assert.Contains(t, expr, `"mediaHealth":{"https://origin.example/dead":{"state":"broken","category":"source_missing","reason":"http_status"}}`)
	assert.NotContains(t, expr, `"https://origin.example/alive":{"state"`)
	require.Len(t, fake.hinted, 1, "hints are asked for once, immediately before the send")
	require.Len(t, fake.hinted[0], 2)
	assert.Equal(t, "https://origin.example/dead", fake.hinted[0][0].Source)
}

func TestDisplayPlaylist_NoHintsLeavesTheFieldAbsent(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/alive"), nil).
		Times(1)
	var expr string
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ = params["expression"].(string)
			return playerOkResponse(), nil
		}).
		Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	fake := &fakeMediaHealth{results: []mediahealth.ProbeResult{{Verdict: mediahealth.VerdictAlive, Status: 200}}}
	commandrouter.SetMediaHealth(ts.handler, fake, ts.logger)

	command := displayPlaylistURLCommand(playlistURL)
	command.Arguments["mediaHealth"] = "injected"
	_, err := ts.handler.Process(ts.ctx, command)
	require.NoError(t, err)
	assert.NotContains(t, expr, "mediaHealth")
}

func TestSourceUnreachableError_RendersReasonCodesNeverURLs(t *testing.T) {
	err := &commandrouter.SourceUnreachableError{Results: []mediahealth.ProbeResult{
		{Source: "https://secret.example/a?…", Verdict: mediahealth.VerdictDead, Status: 200, Reason: mediahealth.ReasonTypeMismatch, Detail: "declared image/png but body is text/html at https://secret.example/a"},
		{Source: "https://secret.example/b", Verdict: mediahealth.VerdictDead, Status: 404, Reason: mediahealth.ReasonHTTPStatus},
		{Source: "https://secret.example/c", Verdict: mediahealth.VerdictDead, Reason: mediahealth.ReasonDNS, Err: errors.New("lookup secret.example: no such host")},
		{Source: "data:…", Verdict: mediahealth.VerdictDead, Reason: mediahealth.ReasonDataURIInvalid},
	}}
	msg := err.Error()
	assert.Contains(t, msg, "item 0: type_mismatch")
	assert.NotContains(t, msg, "HTTP 200", "a content verdict is not an HTTP verdict")
	assert.Contains(t, msg, "item 1: HTTP 404")
	assert.Contains(t, msg, "item 2: dns")
	assert.Contains(t, msg, "item 3: unusable data: URI")
	assert.NotContains(t, msg, "secret.example")
	assert.True(t, commandrouter.IsSourceUnreachable(err))
}

func TestGetMediaHealth(t *testing.T) {
	t.Run("rejects arguments", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()
		commandrouter.SetMediaHealth(ts.handler, &fakeMediaHealth{}, ts.logger)
		result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_MEDIA_HEALTH, Arguments: map[string]interface{}{"x": 1}})
		require.NoError(t, err)
		reply := result.(map[string]interface{})
		assert.Equal(t, false, reply["ok"])
		assert.Contains(t, reply["error"], "no arguments")
	})

	t.Run("unavailable when not wired", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()
		result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_MEDIA_HEALTH})
		require.NoError(t, err)
		reply := result.(map[string]interface{})
		assert.Equal(t, false, reply["ok"])
		assert.Equal(t, "mediaHealthUnavailable", reply["error"])
	})

	t.Run("empty when nothing is on screen", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()
		commandrouter.SetMediaHealth(ts.handler, &fakeMediaHealth{}, ts.logger)
		ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(nil, nil).Times(1)
		result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_MEDIA_HEALTH})
		require.NoError(t, err)
		reply := result.(map[string]interface{})
		assert.Equal(t, true, reply["ok"])
		assert.Equal(t, "empty", reply["status"])
	})

	t.Run("reports one row per item on screen", func(t *testing.T) {
		ts := setup(t)
		defer ts.teardown()
		fake := &reportingMediaHealth{}
		commandrouter.SetMediaHealth(ts.handler, fake, ts.logger)
		items := []dp1playlist.PlaylistItem{
			{ID: "a", Title: "A", Source: "https://origin.example/a"},
			{ID: "b", Title: "B", Source: "https://origin.example/b"},
		}
		ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
			Items:    &items,
			Outcomes: &mediahealth.Outcomes{CastSeq: 7},
		}, nil).Times(1)

		result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_MEDIA_HEALTH})
		require.NoError(t, err)
		reply := result.(map[string]interface{})
		assert.Equal(t, true, reply["ok"])
		assert.Equal(t, "ok", reply["status"])
		assert.Equal(t, 2, reply["total"])
		assert.Equal(t, false, reply["truncated"])
		assert.Equal(t, int64(7), reply["castSeq"])
		rows := reply["items"].([]mediahealth.ReportItem)
		require.Len(t, rows, 2)
		assert.Equal(t, "a", rows[0].ID)
		assert.Equal(t, mediahealth.StateBroken, rows[0].State)
		assert.Equal(t, mediahealth.StateUnknown, rows[1].State)
		require.Len(t, fake.asked, 2)
		assert.Equal(t, "https://origin.example/b", fake.asked[1].Source)
		for _, row := range rows {
			assert.False(t, strings.Contains(row.Title+row.ID, "origin.example"), "no source URL in a row")
		}
	})
}

// reportingMediaHealth answers Report with a broken first row and unknown
// rest, recording what it was asked.
type reportingMediaHealth struct {
	fakeMediaHealth
	asked []dp1playlist.PlaylistItem
}

func (r *reportingMediaHealth) Report(items []dp1playlist.PlaylistItem, _ []int) ([]mediahealth.ReportItem, error) {
	r.asked = items
	rows := make([]mediahealth.ReportItem, len(items))
	for i, it := range items {
		rows[i] = mediahealth.ReportItem{Index: i, ID: it.ID, Title: it.Title, State: mediahealth.StateUnknown}
		if i == 0 {
			rows[i].State = mediahealth.StateBroken
			rows[i].Category = mediahealth.CategorySourceMissing
		}
	}
	return rows, nil
}
