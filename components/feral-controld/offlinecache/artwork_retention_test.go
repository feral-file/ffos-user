package offlinecache

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"syscall"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/require"
)

func TestSharedArtworkRetention(t *testing.T) {
	store := playbackStore(t)
	svc := playbackService(t, store, nil)
	hash, err := store.WriteBlob(strings.NewReader("shared"), 100)
	require.NoError(t, err)
	for _, source := range []string{"one", "two"} {
		require.NoError(t, store.SaveItem(&ItemRecord{Item: dp1playlist.PlaylistItem{Source: source}, Automatic: true, CapturedAt: time.Now(), Resources: []Resource{{SHA256: hash}}}))
	}
	root := t.TempDir()
	dir := filepath.Join(root, SourceKey("software"))
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "asset"), []byte("jpeg"), 0600))
	svc.pruneArtworkRetention(artworkRetention{30, 10}, root)
	keys, err := store.ListItemKeys()
	require.NoError(t, err)
	require.Len(t, keys, 2)
	past := time.Now().AddDate(0, 0, -31)
	require.NoError(t, os.Chtimes(dir, past, past))
	lock, err := os.Open(dir) // #nosec G304 -- test-owned temporary directory
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(lock.Fd()), syscall.LOCK_EX))
	svc.pruneArtworkRetention(artworkRetention{30, 10}, root)
	require.DirExists(t, dir)
	require.NoError(t, lock.Close())
	svc.pruneArtworkRetention(artworkRetention{30, 10}, root)
	require.DirExists(t, dir, "last use survives worker retirement")
	require.NoError(t, os.Chtimes(dir, past, past))
	svc.pruneArtworkRetention(artworkRetention{30, 10}, root)
	require.NoDirExists(t, dir)
	svc.pruneArtworkRetention(artworkRetention{30, 5}, root)
	keys, err = store.ListItemKeys()
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestContinuousPlaybackRefreshesCacheAge(t *testing.T) {
	store := playbackStore(t)
	svc := playbackService(t, store, nil)
	now := svc.clock.Now()
	item := dp1playlist.PlaylistItem{Source: "https://example.com/still.jpg"}
	require.NoError(t, store.SaveItem(&ItemRecord{Item: item, Automatic: true, CapturedAt: now.AddDate(0, 0, -31), Resources: []Resource{{FreshUntil: now.Add(time.Hour)}}}))
	svc.automatic.observe([]dp1playlist.PlaylistItem{item}, 0, true)
	svc.pruneArtworkRetention(artworkRetention{30, 1024}, t.TempDir())
	rec, err := store.LoadItem(SourceKey(item.Source))
	require.NoError(t, err)
	require.False(t, rec.LastPlayedAt.Before(now))
}

// Opt-in live check. Supply one downloaded channel playlist per path.
func TestLiveArtworkPlaylistCache(t *testing.T) {
	paths := os.Getenv("FF_TEST_PLAYLISTS")
	if paths == "" {
		t.Skip("set FF_TEST_PLAYLISTS to playlist JSON paths")
	}
	store := playbackStore(t)
	svc := playbackService(t, store, NewClassifier(newGuardedHTTPClient(net.DefaultResolver, 30*time.Second), net.DefaultResolver))
	svc.started.Store(true)
	broker := NewNativeBroker(store)
	for _, path := range filepath.SplitList(paths) {
		raw, err := os.ReadFile(path) // #nosec G304 G703 -- opt-in local test fixture selected by the test operator
		require.NoError(t, err)
		var list struct {
			Title string
			Items []dp1playlist.PlaylistItem
		}
		require.NoError(t, json.Unmarshal(raw, &list))
		require.NotEmpty(t, list.Items)
		item := list.Items[0]
		svc.automatic.observe(list.Items, 0, false)
		playbackPass(svc)
		_, err = store.LoadItem(SourceKey(item.Source))
		require.ErrorIs(t, err, ErrItemNotFound)
		svc.automatic.observe(list.Items, 0, true)
		playbackPass(svc)
		rec, err := store.LoadItem(SourceKey(item.Source))
		require.NoError(t, err)
		require.True(t, rec.freshForPlayback(time.Now()))
		query, _ := json.Marshal(map[string]any{"source": item.Source, "url": item.Source, "method": "GET", "headers": map[string]string{}})
		for range 2 {
			response := httptest.NewRecorder()
			broker.ServeHTTP(response, httptest.NewRequest("POST", "/v1/open", strings.NewReader(string(query))))
			require.Equal(t, 200, response.Code, response.Body.String())
			data := response.Body.Bytes()
			require.GreaterOrEqual(t, len(data), 4)
			require.EqualValues(t, len(data)-4, binary.BigEndian.Uint32(data[:4]))
			var reply struct{ Path string }
			require.NoError(t, json.Unmarshal(data[4:], &reply))
			require.Equal(t, store.BlobPath(rec.Resources[0].SHA256), reply.Path)
			if probe := os.Getenv("FF_TEST_MEDIA_PROBE"); probe != "" {
				cmd := exec.Command(probe, reply.Path) // #nosec G204 G702 -- optional local test decoder supplied by the test operator
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, string(out))
				t.Log(strings.TrimSpace(string(out)))
			}
		}
		bytes, err := store.BlobSize(rec.Resources[0].SHA256)
		require.NoError(t, err)
		t.Logf("%s: %d bytes captured, two local opens", list.Title, bytes)
	}
}
