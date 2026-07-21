package offlinecache_test

// Store is a thin, atomic wrapper around real filesystem semantics (hash
// verification, temp-file-then-rename, directory sweeps). Exercising it
// against a real temp directory with the real wrapper.OS/JSON
// implementations (rather than mocking every ReadDir/Stat/WriteFile call)
// gives genuine confidence in that behavior; the mock-heavy convention used
// elsewhere in this module is reserved for packages that orchestrate other
// components rather than perform I/O themselves.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
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

// writeBlobString is the test-only equivalent of the []byte-taking
// WriteBlob this store had before it became streaming (WriteBlob now takes
// an io.Reader so capture.go never has to buffer a whole response body —
// see store.go's doc). maxBytes 0 means unlimited, matching WriteBlob's own
// "<=0 means unlimited" contract; tests exercising the limit call
// store.WriteBlob directly instead of through this helper.
func writeBlobString(t *testing.T, store offlinecache.Store, content string) string {
	t.Helper()
	hash, err := store.WriteBlob(strings.NewReader(content), 0)
	require.NoError(t, err)
	return hash
}

// seedItem writes a minimal ready ItemRecord (one HTML resource) directly to
// store, for tests elsewhere in this package (replay_test.go,
// kioskreplay_test.go) that need a pre-cached item without going through the
// full capture pipeline.
func seedItem(t *testing.T, store offlinecache.Store, itemID, blobContent string) offlinecache.Resource {
	t.Helper()
	hash := writeBlobString(t, store, blobContent)

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
	hash1, err := store.WriteBlob(bytes.NewReader(data), 0)
	require.NoError(t, err)
	assert.NotEmpty(t, hash1)

	// Writing identical content again must be a no-op that returns the same hash.
	hash2, err := store.WriteBlob(bytes.NewReader(data), 0)
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

	hash := writeBlobString(t, store, "original")

	// Corrupt the blob on disk directly, bypassing the store's write path.
	corruptPath := filepath.Join(root, "blobs", hash)
	require.NoError(t, wrapper.NewOS().WriteFile(corruptPath, []byte("corrupted"), 0o644))

	_, err := store.ReadBlob(hash)
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

func TestStore_PlaylistURLIndex_SaveAndLoad(t *testing.T) {
	store, _ := newTestStore(t)

	require.NoError(t, store.SavePlaylistURLIndex("https://feed.example.com/p1", "playlist-1"))

	id, err := store.LoadPlaylistIDForURL("https://feed.example.com/p1")
	require.NoError(t, err)
	assert.Equal(t, "playlist-1", id)
}

func TestStore_PlaylistURLIndex_UnknownURLReturnsNotFound(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.LoadPlaylistIDForURL("https://feed.example.com/never-downloaded")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

func TestStore_PlaylistURLIndex_EmptyURLReturnsNotFound(t *testing.T) {
	store, _ := newTestStore(t)
	assert.NoError(t, store.SavePlaylistURLIndex("https://feed.example.com/p1", "playlist-1"))

	_, err := store.LoadPlaylistIDForURL("")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

func TestStore_PlaylistURLIndex_RejectsEmptyArgs(t *testing.T) {
	store, _ := newTestStore(t)

	assert.Error(t, store.SavePlaylistURLIndex("", "playlist-1"))
	assert.Error(t, store.SavePlaylistURLIndex("https://feed.example.com/p1", ""))
}

// TestStore_PlaylistURLIndex_DoesNotLeakIntoListPlaylistIDs pins that the
// index's nested by-url/ subdirectory is invisible to ListPlaylistIDs,
// which only matches "*.json" files directly inside playlistsDir.
func TestStore_PlaylistURLIndex_DoesNotLeakIntoListPlaylistIDs(t *testing.T) {
	store, _ := newTestStore(t)
	require.NoError(t, store.SavePlaylist("playlist-1", json.RawMessage(`{"id":"playlist-1"}`)))
	require.NoError(t, store.SavePlaylistURLIndex("https://feed.example.com/p1", "playlist-1"))

	ids, err := store.ListPlaylistIDs()
	require.NoError(t, err)
	assert.Equal(t, []string{"playlist-1"}, ids)
}

// TestStore_SavePlaylist_ConcurrentDifferentPayloadsNeverCorrupts is the
// regression test for writeFileAtomic's unique-per-call temp file name:
// DownloadPlaylist calls SavePlaylist directly on the caller's own
// goroutine (not serialized through the single-worker capture queue the
// way per-item SaveItem calls are), and the command gate's dedupe only
// collapses byte-identical arguments, so two overlapping
// downloadPlaylist calls for the same playlist id with slightly
// different raw JSON (e.g. a refreshed feed payload) can race here.
// Before writeFileAtomic used CreateTemp, every writer shared the exact
// same fixed path+".tmp" name, so one writer's WriteFile could
// interleave with or be clobbered by another's before either renamed —
// this drives many concurrent, sizeable, DISTINCT payloads at the same
// playlist id and asserts the record that ends up on disk is always
// byte-for-byte exactly one writer's own payload, never a torn mixture
// of two.
func TestStore_SavePlaylist_ConcurrentDifferentPayloadsNeverCorrupts(t *testing.T) {
	store, _ := newTestStore(t)

	const n = 30
	payloads := make([]json.RawMessage, n)
	for i := 0; i < n; i++ {
		// A sizeable payload per writer widens the write's time-in-flight
		// window — this is what actually made the pre-fix shared-tmp-name
		// race reliably reproducible under -race, rather than only
		// theoretically possible with a tiny payload that writes near-
		// instantaneously.
		var items strings.Builder
		for j := 0; j < 500; j++ {
			if j > 0 {
				items.WriteByte(',')
			}
			items.WriteString(fmt.Sprintf(`{"id":"item-%d-%d","source":"https://example.com/%d/%d"}`, i, j, i, j))
		}
		payloads[i] = json.RawMessage(fmt.Sprintf(`{"id":"playlist-1","marker":%d,"items":[%s]}`, i, items.String()))
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(payload json.RawMessage) {
			defer wg.Done()
			assert.NoError(t, store.SavePlaylist("playlist-1", payload))
		}(payloads[i])
	}
	wg.Wait()

	loaded, err := store.LoadPlaylist("playlist-1")
	require.NoError(t, err, "the record on disk must never be left as a corrupted mix of two concurrent writers' bytes")

	matchedWriter := -1
	for i, p := range payloads {
		if string(loaded) == string(p) {
			matchedWriter = i
			break
		}
	}
	assert.GreaterOrEqual(t, matchedWriter, 0,
		"loaded playlist record must exactly match exactly one writer's own payload, never a torn mixture of two: got %d bytes", len(loaded))
}

func TestStore_GC_RemovesOnlyOrphanBlobs(t *testing.T) {
	store, _ := newTestStore(t)

	keepHash := writeBlobString(t, store, "keep me")
	orphanHash := writeBlobString(t, store, "orphan")

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

	writeBlobString(t, store, "12345")
	writeBlobString(t, store, "1234567890")

	usage, err = store.DiskUsage()
	require.NoError(t, err)
	assert.Equal(t, int64(15), usage)
}

// TestStore_SweepIncompleteBlobs_RemovesOnlyTmpFilesAndCountsFreedBytes
// pins the crash-recovery contract: a blobs/*.tmp file left behind by a
// killed process (WriteBlob's own cleanup defer never ran) must be
// removed and its size counted as freed, while a committed blob under
// its content-hash name — even one that GC would itself consider an
// orphan — is left untouched, since SweepIncompleteBlobs' job is
// strictly "reclaim in-progress temp files," not general GC.
func TestStore_SweepIncompleteBlobs_RemovesOnlyTmpFilesAndCountsFreedBytes(t *testing.T) {
	store, root := newTestStore(t)

	committedHash := writeBlobString(t, store, "already committed")

	blobsDir := filepath.Join(root, "blobs")
	require.NoError(t, wrapper.NewOS().MkdirAll(blobsDir, 0o755))
	staleTmpPath := filepath.Join(blobsDir, "incoming-crashed123.tmp")
	require.NoError(t, wrapper.NewOS().WriteFile(staleTmpPath, []byte("half-written"), 0o644))

	removed, freed, err := store.SweepIncompleteBlobs()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.Equal(t, int64(len("half-written")), freed)

	_, statErr := wrapper.NewOS().Stat(staleTmpPath)
	assert.True(t, wrapper.NewOS().IsNotExist(statErr), "the stale temp file must be removed")

	_, err = store.ReadBlob(committedHash)
	assert.NoError(t, err, "a real committed blob must never be touched by the temp-file sweep")
}

// TestStore_SweepIncompleteBlobs_EmptyBlobsDirIsNoop covers the common
// case (a clean prior shutdown, or first-ever startup with no blobs/
// directory yet) so a fresh install/normal restart never logs a
// misleading "swept N files" line.
func TestStore_SweepIncompleteBlobs_EmptyBlobsDirIsNoop(t *testing.T) {
	store, _ := newTestStore(t)

	removed, freed, err := store.SweepIncompleteBlobs()
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.Zero(t, freed)
}

func TestStore_WriteBlob_StreamsWithoutFullyBufferingBody(t *testing.T) {
	store, _ := newTestStore(t)

	// A reader that panics on any call reading more than a small chunk
	// at once would be the sharpest way to prove this, but io.Copy's
	// buffer size is an implementation detail this test should not pin.
	// Instead, this proves the streamed hash/write is correct for a
	// payload assembled from many small chunks — the property that
	// actually matters (capture.go feeds an http.Response.Body, which
	// is exactly this kind of chunked io.Reader, not an in-memory
	// []byte) — rather than asserting on memory usage directly.
	const chunk = "0123456789"
	const repeats = 1000
	r := io.LimitReader(&repeatingReader{chunk: []byte(chunk)}, int64(len(chunk)*repeats))

	hash, err := store.WriteBlob(r, 0)
	require.NoError(t, err)

	got, err := store.ReadBlob(hash)
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat(chunk, repeats), string(got))
}

func TestStore_WriteBlob_RejectsOversizedReaderAndCleansUpTempFile(t *testing.T) {
	store, root := newTestStore(t)

	_, err := store.WriteBlob(strings.NewReader("0123456789"), 5)
	assert.ErrorIs(t, err, offlinecache.ErrBlobTooLarge)

	// No blob must have been committed under any name, and no orphan
	// "incoming-*.tmp" file must be left behind in blobs/ — a leaked
	// temp file here would never be cleaned up by GC (GC deliberately
	// skips ".tmp" names) until a process restart happened to reuse it.
	entries, err := wrapper.NewOS().ReadDir(filepath.Join(root, "blobs"))
	require.NoError(t, err)
	assert.Empty(t, entries, "oversized write must leave blobs/ empty")
}

func TestStore_WriteBlob_ExactlyAtLimitSucceeds(t *testing.T) {
	store, _ := newTestStore(t)

	// maxBytes+1 is read internally to detect an overrun without
	// buffering the whole oversized body; a body exactly at the limit
	// must not be misclassified as having exceeded it.
	hash, err := store.WriteBlob(strings.NewReader("01234"), 5)
	require.NoError(t, err)

	got, err := store.ReadBlob(hash)
	require.NoError(t, err)
	assert.Equal(t, "01234", string(got))
}

// repeatingReader emits chunk repeatedly, ignoring EOF, until the caller's
// io.LimitReader wrapper cuts it off — used to feed WriteBlob a payload
// that arrives as many small reads rather than one contiguous []byte.
type repeatingReader struct {
	chunk []byte
	pos   int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		p[n] = r.chunk[r.pos]
		r.pos = (r.pos + 1) % len(r.chunk)
		n++
	}
	return n, nil
}
