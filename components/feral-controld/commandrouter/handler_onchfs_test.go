package commandrouter_test

import (
	"context"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

// fakeOnchfs records each Ensure; order is shared with orderedProber so a
// test can see which ran first.
type fakeOnchfs struct {
	order *[]string
	items [][]dp1playlist.PlaylistItem
	wait  time.Duration
}

func (f *fakeOnchfs) Ensure(_ context.Context, items []dp1playlist.PlaylistItem, wait time.Duration) int {
	*f.order = append(*f.order, "onchfs")
	f.items = append(f.items, items)
	f.wait = wait
	return 0
}

type orderedProber struct {
	fakeMediaHealth
	order *[]string
}

func (p *orderedProber) Preflight(ctx context.Context, sources []string) []mediahealth.ProbeResult {
	*p.order = append(*p.order, "preflight")
	return p.fakeMediaHealth.Preflight(ctx, sources)
}

// A cast reads its onchfs works from chain before the preflight and before
// anything reaches the player, with the items' device-local sources.
func TestCommandHandler_Process_DisplayPlaylist_ReadsOnchfsWorksBeforeThePreflight(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	onchfsSource := dp1.OnchfsSource("onchfs://1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a/?fxhash=0x06ec")
	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL, gomock.Any()).
		Return(probeTestPlaylist(onchfsSource, "https://origin.example/dead"), dp1.CastPlan{}, nil).
		Times(1)

	var order []string
	materializer := &fakeOnchfs{order: &order}
	commandrouter.SetOnchfsMaterializer(ts.handler, materializer, ts.logger)
	// Both dead keeps the test away from the player: the cast is rejected
	// after the preflight, and only the order is under test.
	prober := &orderedProber{order: &order, fakeMediaHealth: fakeMediaHealth{results: []mediahealth.ProbeResult{
		{Source: onchfsSource, Verdict: mediahealth.VerdictDead, Status: 404},
		{Source: "https://origin.example/dead", Verdict: mediahealth.VerdictDead, Status: 404},
	}}}
	commandrouter.SetMediaHealth(ts.handler, prober, ts.logger)

	_, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))
	require.Error(t, err)

	assert.Equal(t, []string{"onchfs", "preflight"}, order)
	require.Len(t, materializer.items, 1)
	assert.Equal(t, onchfsSource, materializer.items[0][0].Source)
	assert.Positive(t, materializer.wait)
}
