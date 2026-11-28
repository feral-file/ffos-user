package offlinecache

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestNativePlaylistCacheCountsPlayableSourcesOnce(t *testing.T) {
	const source = "https://example.org/art"
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	cache := NewNativePlaybackCache(store)
	record := &ItemRecord{Entry: source}
	record.Item.Source = source
	entry := Resource{URL: source, Status: 200, ContentType: "text/html", BrowserPolicyVersion: 2}
	asset := Resource{URL: source + "/script", Status: 200, BrowserPolicyVersion: 2}
	expired := asset
	expired.FreshUntil = time.Unix(1, 0)
	image := entry
	image.ContentType = "image/png"
	image.BrowserPolicyVersion = 0
	oldHeaders := asset
	oldHeaders.BrowserPolicyVersion = 0
	for _, tc := range []struct {
		name      string
		resources []Resource
		want      int
	}{
		{"complete web capture", []Resource{entry, asset}, 1},
		{"headerless POST is a coverage miss", []Resource{entry, asset, {Method: "POST", URL: source + "/analytics"}}, 1},
		{"missing entry", []Resource{asset}, 0},
		{"expired subresource", []Resource{entry, expired}, 0},
		{"legacy subresource headers", []Resource{entry, oldHeaders}, 0},
		{"standalone image", []Resource{image}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record.Resources = tc.resources
			require.NoError(t, store.SaveItem(record))
			count, err := cache.SyncPlaylist(t.Context(), []string{source, "https://example.org/missing", source})
			require.NoError(t, err)
			require.Equal(t, tc.want, count)
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := cache.SyncPlaylist(ctx, []string{source})
	require.ErrorIs(t, err, context.Canceled)
}
