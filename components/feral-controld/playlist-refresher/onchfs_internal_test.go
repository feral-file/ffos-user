package refresher

import (
	"context"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

type recordingOnchfs struct {
	items [][]dp1playlist.PlaylistItem
	waits []time.Duration
}

func (r *recordingOnchfs) Ensure(_ context.Context, items []dp1playlist.PlaylistItem, wait time.Duration) int {
	r.items = append(r.items, items)
	r.waits = append(r.waits, wait)
	return 0
}

// A refresh that brings onchfs works starts reading them without waiting
// (the scope sync runs under the playback lock), then syncs scope as before.
func TestSyncReplayScopeStartsOnchfsReadsWithoutWaiting(t *testing.T) {
	ctrl := gomock.NewController(t)
	kr := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	source := dp1.OnchfsSource("onchfs://1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a")
	kr.EXPECT().SyncPlaylist(gomock.Any(), []string{source}).Return(0, nil).Times(1)
	kr.EXPECT().MarkPlaybackChanged().Times(1)
	m := &recordingOnchfs{}
	r := &refresher{context: context.Background(), kioskReplay: kr, logger: zaptest.NewLogger(t)}
	SetOnchfsMaterializer(r, m, zaptest.NewLogger(t))

	r.syncReplayScopeLocked(&dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{Source: source}}}})

	require.Len(t, m.items, 1)
	assert.Equal(t, source, m.items[0][0].Source)
	assert.Zero(t, m.waits[0])
}
