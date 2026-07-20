package offlinecache_test

// Store is a thin, atomic wrapper around real filesystem semantics (hash
// verification, temp-file-then-rename, directory sweeps). Exercising it
// against a real temp directory with the real wrapper.OS/JSON
// implementations (rather than mocking every ReadDir/Stat/WriteFile call)
// gives genuine confidence in that behavior; the mock-heavy convention used
// elsewhere in this module is reserved for packages that orchestrate other
// components rather than perform I/O themselves.

import (
	"encoding/json"
	"path/filepath"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func newTestStore(t *testing.T) (offlinecache.Store, string) {
	root := t.TempDir()
	logger := zaptest.NewLogger(t)
	store := offlinecache.NewStore(root, wrapper.NewOS(), wrapper.NewJSON(), logger)
	return store, root
}

// seedItem writes a minimal ready ItemRecord (one HTML resource) directly to
// store, for tests elsewhere in this package (replay_test.go,
// kioskreplay_test.go) that need a pre-cached item without going through the
// full capture pipeline.
func seedItem(t *testing.T, store offlinecache.Store, itemID, blobContent string) offlinecache.Resource {
	t.Helper()
	hash, err := store.WriteBlob([]byte(blobContent))
	require.NoError(t, err)

	res := offlinecache.Resource{
		URL:         "https://example.com/" + itemID + "/index.html",
		Status:      200,
		SHA256:      hash,
		ContentType: "text/html",
	}
	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		ItemID:    itemID,
		Item:      dp1playlist.PlaylistItem{ID: itemID, Source: res.URL},
		Entry:     res.URL,
		Resources: []offlinecache.Resource{res},
		Coverage:  offlinecache.Coverage{Complete: true},
	}))
	return res
}

func TestStore_WriteBlob_DedupAndVerify(t *testing.T) {
	store, root := newTestStore(t)

	data := []byte("hello offline world")
	hash1, err := store.WriteBlob(data)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1)

	// Writing identical content again must be a no-op that returns the same hash.
	hash2, err := store.WriteBlob(data)
	require.NoError(t, err)
	assert.Equal(t, hash1, hash2)

	got, err := store.ReadBlob(hash1)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	size, err := store.BlobSize(hash1)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)

	assert.Equal(t, filepath.Join(root, "blobs", hash1), store.BlobPath(hash1))
}

func TestStore_ReadBlob_NotFound(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.ReadBlob("does-not-exist")
	assert.ErrorIs(t, err, offlinecache.ErrBlobNotFound)

	_, err = store.BlobSize("does-not-exist")
	assert.ErrorIs(t, err, offlinecache.ErrBlobNotFound)
}

func TestStore_ReadBlob_HashMismatch(t *testing.T) {
	store, root := newTestStore(t)

	hash, err := store.WriteBlob([]byte("original"))
	require.NoError(t, err)

	// Corrupt the blob on disk directly, bypassing the store's write path.
	corruptPath := filepath.Join(root, "blobs", hash)
	require.NoError(t, wrapper.NewOS().WriteFile(corruptPath, []byte("corrupted"), 0o644))

	_, err = store.ReadBlob(hash)
	assert.ErrorIs(t, err, offlinecache.ErrBlobHashMismatch)
}

func TestStore_Item_SaveLoadDelete(t *testing.T) {
	store, _ := newTestStore(t)

	rec := &offlinecache.ItemRecord{
		ItemID: "item-1",
		Item:   dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"},
		Entry:  "https://example.com/index.html",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/index.html", Status: 200, SHA256: "abc123", ContentType: "text/html"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}

	require.NoError(t, store.SaveItem(rec))

	loaded, err := store.LoadItem("item-1")
	require.NoError(t, err)
	assert.Equal(t, rec.ItemID, loaded.ItemID)
	assert.Equal(t, rec.Item.Source, loaded.Item.Source)
	assert.Equal(t, rec.Resources, loaded.Resources)
	assert.True(t, loaded.Coverage.Complete)

	ids, err := store.ListItemIDs()
	require.NoError(t, err)
	assert.Equal(t, []string{"item-1"}, ids)

	require.NoError(t, store.DeleteItem("item-1"))

	_, err = store.LoadItem("item-1")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)

	ids, err = store.ListItemIDs()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestStore_LoadItem_NotFound(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.LoadItem("missing")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
}

func TestStore_SaveItem_RequiresItemID(t *testing.T) {
	store, _ := newTestStore(t)

	err := store.SaveItem(&offlinecache.ItemRecord{})
	assert.Error(t, err)
}

func TestStore_UnsafeID_Rejected(t *testing.T) {
	store, _ := newTestStore(t)

	tests := []string{"../escape", "a/b", "."}
	for _, id := range tests {
		t.Run(id, func(t *testing.T) {
			err := store.SaveItem(&offlinecache.ItemRecord{ItemID: id})
			assert.Error(t, err)
		})
	}
}

func TestStore_Playlist_SaveLoadDelete(t *testing.T) {
	store, _ := newTestStore(t)

	raw := json.RawMessage(`{"id":"playlist-1","items":[]}`)
	require.NoError(t, store.SavePlaylist("playlist-1", raw))

	loaded, err := store.LoadPlaylist("playlist-1")
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(loaded))

	ids, err := store.ListPlaylistIDs()
	require.NoError(t, err)
	assert.Equal(t, []string{"playlist-1"}, ids)

	require.NoError(t, store.DeletePlaylist("playlist-1"))

	_, err = store.LoadPlaylist("playlist-1")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

func TestStore_GC_RemovesOnlyOrphanBlobs(t *testing.T) {
	store, _ := newTestStore(t)

	keepHash, err := store.WriteBlob([]byte("keep me"))
	require.NoError(t, err)
	orphanHash, err := store.WriteBlob([]byte("orphan"))
	require.NoError(t, err)

	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		ItemID: "item-1",
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/a.js", Status: 200, SHA256: keepHash},
		},
	}))

	removed, freed, err := store.GC()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.Equal(t, int64(len("orphan")), freed)

	_, err = store.ReadBlob(keepHash)
	assert.NoError(t, err)

	_, err = store.ReadBlob(orphanHash)
	assert.ErrorIs(t, err, offlinecache.ErrBlobNotFound)
}

func TestStore_DiskUsage(t *testing.T) {
	store, _ := newTestStore(t)

	usage, err := store.DiskUsage()
	require.NoError(t, err)
	assert.Zero(t, usage)

	_, err = store.WriteBlob([]byte("12345"))
	require.NoError(t, err)
	_, err = store.WriteBlob([]byte("1234567890"))
	require.NoError(t, err)

	usage, err = store.DiskUsage()
	require.NoError(t, err)
	assert.Equal(t, int64(15), usage)
}
