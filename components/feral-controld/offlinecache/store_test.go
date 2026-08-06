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
	"io/fs"
	go_os "os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// sourceFor derives the deterministic source URL seedItem stores a record
// under for a short test-local name, so tests can reference a seeded item
// by name while still exercising the real source-keyed lookups
// (offlinecache.SourceKey(sourceFor(name)) is the record's key).
func sourceFor(name string) string {
	return "https://example.com/" + name + "/index.html"
}

// seedItem writes a minimal ready ItemRecord (one HTML resource) directly to
// store, for tests elsewhere in this package (replay_test.go,
// kioskreplay_test.go) that need a pre-cached item without going through the
// full capture pipeline. name is a short test-local handle; the record's
// real identity is sourceFor(name) — the DP-1 id is set to name only to
// mirror production records, which carry the id informationally.
func seedItem(t *testing.T, store offlinecache.Store, name, blobContent string) offlinecache.Resource {
	t.Helper()
	hash := writeBlobString(t, store, blobContent)

	res := offlinecache.Resource{
		URL:         sourceFor(name),
		Status:      200,
		SHA256:      hash,
		ContentType: "text/html",
	}
	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		Item:      dp1playlist.PlaylistItem{ID: name, Source: res.URL},
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

	// Writing identical content again converges on the same hash — that
	// convergence IS the dedup, not an early return: the blob is rewritten
	// rather than skipped (see
	// TestStore_WriteBlob_RepairsCorruptExistingBlob for why that
	// difference is load-bearing).
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

	source := "https://example.com/index.html"
	key := offlinecache.SourceKey(source)
	rec := &offlinecache.ItemRecord{
		Item:  dp1playlist.PlaylistItem{ID: "item-1", Source: source},
		Entry: source,
		Resources: []offlinecache.Resource{
			{URL: source, Status: 200, SHA256: "abc123", ContentType: "text/html"},
		},
		Coverage: offlinecache.Coverage{Complete: true},
	}

	require.NoError(t, store.SaveItem(rec))

	loaded, err := store.LoadItem(key)
	require.NoError(t, err)
	assert.Equal(t, rec.Item.Source, loaded.Item.Source)
	assert.Equal(t, rec.Resources, loaded.Resources)
	assert.True(t, loaded.Coverage.Complete)

	keys, err := store.ListItemKeys()
	require.NoError(t, err)
	assert.Equal(t, []string{key}, keys)

	removed, err := store.DeleteItem(key)
	require.NoError(t, err)
	assert.True(t, removed, "deleting an existing record must report that it removed one")

	_, err = store.LoadItem(key)
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)

	keys, err = store.ListItemKeys()
	require.NoError(t, err)
	assert.Empty(t, keys)

	// Remove-if-exists: a second delete is still success, but must NOT
	// claim it removed anything — Service reads that flag to decide whether
	// a clear settled the item at not_cached (see ClearItem).
	removed, err = store.DeleteItem(key)
	require.NoError(t, err)
	assert.False(t, removed, "deleting an already-absent record must report that it removed nothing")
}

func TestStore_LoadItem_NotFound(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.LoadItem(offlinecache.SourceKey("https://example.com/never-cached"))
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
}

func TestStore_SaveItem_RequiresSource(t *testing.T) {
	store, _ := newTestStore(t)

	err := store.SaveItem(&offlinecache.ItemRecord{})
	assert.Error(t, err)
}

// TestStore_SourceKey_HostileSourcesArePathSafe pins that identity keying
// makes item filenames immune to path-hostile source strings: the record
// lands under a hex hash regardless of what the source contains, and
// round-trips through Save/Load/Delete.
func TestStore_SourceKey_HostileSourcesArePathSafe(t *testing.T) {
	store, root := newTestStore(t)

	sources := []string{
		"../../etc/passwd",
		"https://host/a/b?c=d&e=f#frag",
		"file:///dev/null",
		strings.Repeat("https://very-long-url.example.com/segment/", 50),
	}
	for _, source := range sources {
		t.Run(source[:min(len(source), 40)], func(t *testing.T) {
			key := offlinecache.SourceKey(source)
			require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
				Item: dp1playlist.PlaylistItem{Source: source},
			}))

			// The record must live at items/<key>.json inside the root —
			// never anywhere a hostile source could steer it.
			_, statErr := go_os.Stat(filepath.Join(root, "items", key+".json"))
			require.NoError(t, statErr)

			loaded, err := store.LoadItem(key)
			require.NoError(t, err)
			assert.Equal(t, source, loaded.Item.Source)

			removed, err := store.DeleteItem(key)
			require.NoError(t, err)
			assert.True(t, removed)
		})
	}
}

// TestStore_LoadItem_RejectsFilenameContentIdentityMismatch pins
// LoadItem's identity check: a parseable record sitting at a valid key
// that is NOT SourceKey of its own source must be rejected as corrupt
// (deterministic), never returned — its content is not what this key's
// readers believe it is, and replay/status keyed off it would describe
// the wrong source.
func TestStore_LoadItem_RejectsFilenameContentIdentityMismatch(t *testing.T) {
	store, root := newTestStore(t)

	rec := &offlinecache.ItemRecord{
		Item:     dp1playlist.PlaylistItem{Source: "https://example.com/real-source"},
		Coverage: offlinecache.Coverage{Complete: true},
	}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	wrongKey := offlinecache.SourceKey("https://example.com/some-other-source")
	require.NoError(t, wrapper.NewOS().MkdirAll(filepath.Join(root, "items"), 0o755))
	require.NoError(t, wrapper.NewOS().WriteFile(
		filepath.Join(root, "items", wrongKey+".json"), data, 0o644))

	_, err = store.LoadItem(wrongKey)
	assert.ErrorIs(t, err, offlinecache.ErrItemRecordCorrupt)
}

// TestStore_GC_QuarantinesIdentityMismatchedRecord pins GC's half of the
// same check: a parseable record under a valid-but-wrong key is
// unreachable by every reader (LoadItem rejects it), so GC must
// quarantine it and reclaim its exclusive blobs rather than keep them
// pinned for content nothing can serve.
func TestStore_GC_QuarantinesIdentityMismatchedRecord(t *testing.T) {
	store, root := newTestStore(t)

	pinnedHash := writeBlobString(t, store, "referenced only by the mismatched record")
	rec := &offlinecache.ItemRecord{
		Item:      dp1playlist.PlaylistItem{Source: "https://example.com/real-source"},
		Resources: []offlinecache.Resource{{URL: "https://example.com/a.js", Status: 200, SHA256: pinnedHash}},
	}
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	wrongKey := offlinecache.SourceKey("https://example.com/some-other-source")
	wrongPath := filepath.Join(root, "items", wrongKey+".json")
	require.NoError(t, wrapper.NewOS().MkdirAll(filepath.Join(root, "items"), 0o755))
	require.NoError(t, wrapper.NewOS().WriteFile(wrongPath, data, 0o644))

	removed, _, err := store.GC()
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the mismatched record's exclusive blob is reclaimed")

	_, statErr := wrapper.NewOS().Stat(wrongPath)
	assert.True(t, wrapper.NewOS().IsNotExist(statErr))
	_, statErr = wrapper.NewOS().Stat(wrongPath + ".corrupt")
	assert.NoError(t, statErr, "the mismatched record's bytes must be preserved for forensics")
}

// TestStore_LoadItem_RejectsRawSource pins the boundary contract: item ops
// take a SourceKey, never a raw source URL — passing one is a programming
// bug and must fail loudly (not resolve, not traverse).
func TestStore_LoadItem_RejectsRawSource(t *testing.T) {
	store, _ := newTestStore(t)

	for _, bad := range []string{"https://example.com/art", "../escape", "", "ABCDEF" + strings.Repeat("0", 58)} {
		_, err := store.LoadItem(bad)
		require.Error(t, err)
		assert.NotErrorIs(t, err, offlinecache.ErrItemNotFound,
			"a malformed key must fail as invalid input, not report a clean miss")

		_, err = store.DeleteItem(bad)
		assert.Error(t, err)
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
			fmt.Fprintf(&items, `{"id":"item-%d-%d","source":"https://example.com/%d/%d"}`, i, j, i, j)
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

// TestStore_WriteBlob_RepairsCorruptExistingBlob pins that a re-download
// of content whose blob already exists on disk REPLACES the stored file
// rather than short-circuiting on existence. WriteBlob previously treated
// "final path exists" as "already stored" — after a power-loss torn write
// (rename survived, data didn't) that dedup made the corruption
// permanent: every fresh good copy was discarded while ReadBlob's hash
// verification kept rejecting the stored bytes, so the item reported
// ready and never played.
func TestStore_WriteBlob_RepairsCorruptExistingBlob(t *testing.T) {
	store, _ := newTestStore(t)

	data := []byte("artwork bytes")
	hash, err := store.WriteBlob(bytes.NewReader(data), 0)
	require.NoError(t, err)

	// Simulate the torn write: the content-addressed file exists but its
	// bytes are gone (zero-length is what ext4 delayed allocation
	// typically leaves behind).
	require.NoError(t, wrapper.NewOS().WriteFile(store.BlobPath(hash), nil, 0o644))
	_, err = store.ReadBlob(hash)
	require.ErrorIs(t, err, offlinecache.ErrBlobHashMismatch)

	// The next capture referencing the same content must repair it.
	rehash, err := store.WriteBlob(bytes.NewReader(data), 0)
	require.NoError(t, err)
	assert.Equal(t, hash, rehash)

	got, err := store.ReadBlob(hash)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestStore_GC_RemovesOnlyOrphanBlobs(t *testing.T) {
	store, _ := newTestStore(t)

	keepHash := writeBlobString(t, store, "keep me")
	orphanHash := writeBlobString(t, store, "orphan")

	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: "https://example.com/item-1"},
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

// TestStore_GC_QuarantinesCorruptRecordAndKeepsSweeping pins the
// deterministic half of GC's unreadable-record split: a record whose
// bytes read fine but do not parse can never be fixed by retrying, and
// aborting on it forever would wedge GC — the only path that frees blob
// bytes — permanently disabling the disk budget. It must instead be
// quarantined (renamed out of ListItemKeys' view, bytes preserved) and
// the sweep must proceed: blobs shared with readable records survive via
// those records' keep-set entries, while blobs only the corrupt record
// referenced are unreachable by every reader and are reclaimed.
func TestStore_GC_QuarantinesCorruptRecordAndKeepsSweeping(t *testing.T) {
	store, root := newTestStore(t)

	sharedHash := writeBlobString(t, store, "shared with a readable record")
	exclusiveHash := writeBlobString(t, store, "referenced only by the corrupt record")
	orphanHash := writeBlobString(t, store, "genuine orphan")

	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: "https://example.com/item-healthy"},
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/a.js", Status: 200, SHA256: sharedHash},
		},
	}))

	itemsDir := filepath.Join(root, "items")
	corruptPath := filepath.Join(itemsDir, offlinecache.SourceKey("https://example.com/item-corrupt")+".json")
	require.NoError(t, wrapper.NewOS().WriteFile(corruptPath, []byte("{not json"), 0o644))

	removed, _, err := store.GC()
	require.NoError(t, err)
	assert.Equal(t, 2, removed, "the orphan and the corrupt record's exclusive blob are both reclaimed")

	_, statErr := wrapper.NewOS().Stat(corruptPath)
	assert.True(t, wrapper.NewOS().IsNotExist(statErr), "the corrupt record must leave ListItemKeys' view")
	_, statErr = wrapper.NewOS().Stat(corruptPath + ".corrupt")
	assert.NoError(t, statErr, "the corrupt record's bytes must be preserved for forensics")

	_, err = store.ReadBlob(sharedHash)
	assert.NoError(t, err, "a blob shared with a readable record must survive")
	_, err = store.ReadBlob(exclusiveHash)
	assert.ErrorIs(t, err, offlinecache.ErrBlobNotFound)
	_, err = store.ReadBlob(orphanHash)
	assert.ErrorIs(t, err, offlinecache.ErrBlobNotFound)

	// The quarantine is what makes the NEXT pass clean — a wedge would
	// show up here as the same warn/quarantine cycle or an error.
	removed, _, err = store.GC()
	require.NoError(t, err)
	assert.Zero(t, removed)
}

// TestStore_GC_RemovesRecordAtInvalidKeyFilename pins the invalid-name
// branch: a .json record whose filename is not a valid source key —
// however well its bytes parse — is unreachable by every reader (LoadItem
// validates keys, ListItemKeys filters names), so GC must retire it and
// reclaim its exclusive blobs rather than pin them forever or, worse,
// treat the permanently-invalid name as a transient error and wedge every
// future sweep. This is the path that retires a legacy id-keyed cache
// wholesale on the first boot after the upgrade.
func TestStore_GC_RemovesRecordAtInvalidKeyFilename(t *testing.T) {
	store, root := newTestStore(t)

	strayHash := writeBlobString(t, store, "referenced only by the stray record")

	// A perfectly parsable record, but at a legacy-style id filename.
	strayRec := &offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: "https://example.com/stray"},
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/stray.js", Status: 200, SHA256: strayHash},
		},
	}
	data, err := json.Marshal(strayRec)
	require.NoError(t, err)
	strayPath := filepath.Join(root, "items", "legacy-item-id.json")
	require.NoError(t, wrapper.NewOS().MkdirAll(filepath.Join(root, "items"), 0o755))
	require.NoError(t, wrapper.NewOS().WriteFile(strayPath, data, 0o644))

	keys, err := store.ListItemKeys()
	require.NoError(t, err)
	assert.Empty(t, keys, "an invalid-key filename must be invisible to ordinary readers")

	removed, _, err := store.GC()
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the stray record's exclusive blob is reclaimed")

	// Deleted, NOT quarantined: this is the bulk shape of a
	// pre-source-keying cache, and a whole store's worth of *.corrupt
	// files would sit inside the maxDiskBytes budget with nothing able to
	// reclaim them (DiskUsage counts them; DeleteItem and eviction only
	// reach <key>.json). Quarantine is reserved for genuine anomalies —
	// see TestStore_GC_QuarantinesIdentityMismatchedRecord.
	_, statErr := wrapper.NewOS().Stat(strayPath)
	assert.True(t, wrapper.NewOS().IsNotExist(statErr))
	_, statErr = wrapper.NewOS().Stat(strayPath + ".corrupt")
	assert.True(t, wrapper.NewOS().IsNotExist(statErr),
		"an unreadable-by-name legacy record must be removed outright, not left as permanent residue inside the disk budget")

	// And the budget genuinely reflects that: nothing of the stray
	// record survives to be counted.
	usage, err := store.DiskUsage()
	require.NoError(t, err)
	assert.Zero(t, usage, "neither the stray record nor its blob may still count against maxDiskBytes")
}

// failReadOS delegates to a real OS wrapper but fails ReadFile for one
// specific path, simulating a transient per-file I/O error (EIO/EMFILE)
// that a later retry could recover from.
type failReadOS struct {
	wrapper.OS
	failPath string
}

func (f *failReadOS) ReadFile(path string) ([]byte, error) {
	if path == f.failPath {
		return nil, fmt.Errorf("simulated transient read error for %s", path)
	}
	return f.OS.ReadFile(path)
}

// TestStore_GC_AbortsOnTransientlyUnreadableRecord pins the other half
// of the split: a record that cannot be READ (as opposed to parsed) may
// recover on a later attempt, and it still references blobs — skipping
// it would narrow the keep-set and delete those blobs as "orphans",
// losing good data unrecoverably. The whole sweep must abort, removing
// NOTHING (not even genuine orphans): an aborted pass costs only disk
// until a retry succeeds.
func TestStore_GC_AbortsOnTransientlyUnreadableRecord(t *testing.T) {
	root := t.TempDir()
	flakySource := "https://example.com/item-flaky"
	failOS := &failReadOS{OS: wrapper.NewOS(), failPath: filepath.Join(root, "items", offlinecache.SourceKey(flakySource)+".json")}
	store := offlinecache.NewStore(root, failOS, wrapper.NewJSON(), zaptest.NewLogger(t))

	referencedHash := writeBlobString(t, store, "referenced by the unreadable record")
	orphanHash := writeBlobString(t, store, "genuine orphan")

	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		Item: dp1playlist.PlaylistItem{Source: flakySource},
		Resources: []offlinecache.Resource{
			{URL: "https://example.com/a.js", Status: 200, SHA256: referencedHash},
		},
	}))

	_, _, err := store.GC()
	require.Error(t, err)

	_, err = store.ReadBlob(referencedHash)
	assert.NoError(t, err, "blobs referenced by the unreadable record must survive")
	_, err = store.ReadBlob(orphanHash)
	assert.NoError(t, err, "an aborted sweep must not remove anything, orphans included")

	// Once the transient error clears, the same store recovers on its own.
	failOS.failPath = ""
	removed, _, err := store.GC()
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "only the genuine orphan is reclaimed after recovery")
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

// blobBytesOnDisk sums only the blob payloads under a store root. Store.
// DiskUsage deliberately counts item and playlist records too (see its
// doc), so a test whose subject is specifically "how many blob bytes did
// this write" needs its own measure rather than reusing that one.
func blobBytesOnDisk(t *testing.T, root string) int64 {
	t.Helper()
	entries, err := go_os.ReadDir(filepath.Join(root, "blobs"))
	if go_os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	var total int64
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		info, err := e.Info()
		require.NoError(t, err)
		total += info.Size()
	}
	return total
}

// allCacheBytesOnDisk walks the whole store root, which is an independent
// way of arriving at what DiskUsage sums directory by directory — so an
// assertion using it actually cross-checks that accounting rather than
// restating it.
func allCacheBytesOnDisk(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasSuffix(d.Name(), ".tmp") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	}))
	return total
}

// TestStore_DiskUsage_CountsRecordsNotJustBlobs pins the accounting half
// of the maxDiskBytes fix. DiskUsage once summed blobs/ alone, so
// playlist bodies and item records — real persisted cache data — sat
// entirely outside the configured ceiling.
func TestStore_DiskUsage_CountsRecordsNotJustBlobs(t *testing.T) {
	store, root := newTestStore(t)

	require.NoError(t, store.SavePlaylist("playlist-1", json.RawMessage(`{"id":"playlist-1","items":[]}`)))
	require.NoError(t, store.SavePlaylistURLIndex("https://feed.example.com/p1", "playlist-1"))

	usage, err := store.DiskUsage()
	require.NoError(t, err)
	assert.Zero(t, blobBytesOnDisk(t, root), "fixture writes no blobs at all")
	assert.Positive(t, usage, "a store holding only metadata must still report usage, or the budget cannot see it")
	assert.Equal(t, allCacheBytesOnDisk(t, root), usage)
}

// TestStore_PrunePlaylistRecords_BoundsMetadataGrowth is the regression
// test for the unbounded-metadata finding: a device asked to download a
// series of distinct playlists whose items are all non-cacheable writes
// a playlist body every time and queues nothing, so no capture ever runs
// and eviction — which only walks items and their blobs — never sees any
// of it.
func TestStore_PrunePlaylistRecords_BoundsMetadataGrowth(t *testing.T) {
	store, root := newTestStore(t)

	const written = 40
	const keep = 10
	for i := 0; i < written; i++ {
		id := fmt.Sprintf("playlist-%02d", i)
		require.NoError(t, store.SavePlaylist(id, json.RawMessage(`{"id":"`+id+`","items":[]}`)))
		require.NoError(t, store.SavePlaylistURLIndex("https://feed.example.com/"+id, id))
		// Distinct mtimes, oldest first, so "which ones survive" is a
		// question about age rather than about filesystem timestamp
		// granularity.
		path := filepath.Join(root, "playlists", id+".json")
		age := time.Duration(written-i) * time.Minute
		require.NoError(t, go_os.Chtimes(path, time.Now().Add(-age), time.Now().Add(-age)))
	}

	beforeUsage, err := store.DiskUsage()
	require.NoError(t, err)

	removed, err := store.PrunePlaylistRecords(keep)
	require.NoError(t, err)
	assert.Equal(t, written-keep, removed)

	ids, err := store.ListPlaylistIDs()
	require.NoError(t, err)
	require.Len(t, ids, keep, "metadata must be bounded by the retention limit")

	// The newest survive; the oldest are gone.
	for i := 0; i < written-keep; i++ {
		_, err := store.LoadPlaylist(fmt.Sprintf("playlist-%02d", i))
		assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound, "oldest playlist bodies must be pruned first")
	}
	for i := written - keep; i < written; i++ {
		_, err := store.LoadPlaylist(fmt.Sprintf("playlist-%02d", i))
		assert.NoError(t, err, "the most recent playlists must survive")
	}

	// The by-url index is pruned alongside, or it would grow unbounded
	// on its own while every lookup through it failed closed.
	_, err = store.LoadPlaylistIDForURL("https://feed.example.com/playlist-00")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
	id, err := store.LoadPlaylistIDForURL(fmt.Sprintf("https://feed.example.com/playlist-%02d", written-1))
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("playlist-%02d", written-1), id)

	afterUsage, err := store.DiskUsage()
	require.NoError(t, err)
	assert.Less(t, afterUsage, beforeUsage, "pruning must actually reclaim accounted bytes")
}

// TestStore_EmptySourceRecordIsCorrupt covers the one identity failure the
// filename/content check cannot catch on its own: a record whose source is
// empty, stored under SourceKey(""). That name is a perfectly well-formed
// hash and the record genuinely agrees with its own (empty) content, so
// the mismatch check passes it. It is corrupt regardless — SaveItem
// refuses to write one, so it cannot have come from this code, and the
// source is the identity every reader keys on. Left readable it would put
// an entry with an empty Source into status, which no client can match,
// and GC would keep it and its blobs inside the disk budget forever.
//
// Both readers are asserted because they are separate paths: LoadItem
// validates, while GC reads through loadItemByName and re-derives its own
// verdict.
func TestStore_EmptySourceRecordIsCorrupt(t *testing.T) {
	emptyKey := offlinecache.SourceKey("")

	t.Run("LoadItem rejects it as corrupt", func(t *testing.T) {
		store, root := newTestStore(t)
		rec := &offlinecache.ItemRecord{
			Item: dp1playlist.PlaylistItem{Source: ""},
		}
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		require.NoError(t, wrapper.NewOS().MkdirAll(filepath.Join(root, "items"), 0o755))
		require.NoError(t, wrapper.NewOS().WriteFile(
			filepath.Join(root, "items", emptyKey+".json"), data, 0o644))

		_, err = store.LoadItem(emptyKey)
		require.Error(t, err)
		assert.ErrorIs(t, err, offlinecache.ErrItemRecordCorrupt,
			"an empty source is an identity failure, not a clean miss")
	})

	t.Run("GC quarantines it and reclaims its blobs", func(t *testing.T) {
		store, root := newTestStore(t)
		pinnedHash := writeBlobString(t, store, "referenced only by the empty-source record")
		rec := &offlinecache.ItemRecord{
			Item:      dp1playlist.PlaylistItem{Source: ""},
			Resources: []offlinecache.Resource{{URL: "https://example.com/a.js", Status: 200, SHA256: pinnedHash}},
		}
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		path := filepath.Join(root, "items", emptyKey+".json")
		require.NoError(t, wrapper.NewOS().MkdirAll(filepath.Join(root, "items"), 0o755))
		require.NoError(t, wrapper.NewOS().WriteFile(path, data, 0o644))

		removed, _, err := store.GC()
		require.NoError(t, err)
		assert.Equal(t, 1, removed, "the empty-source record's exclusive blob is reclaimed")

		_, statErr := wrapper.NewOS().Stat(path)
		assert.True(t, wrapper.NewOS().IsNotExist(statErr), "the record must not be left in place")
		_, statErr = wrapper.NewOS().Stat(path + ".corrupt")
		assert.NoError(t, statErr, "its bytes must be preserved for forensics")
	})

	t.Run("SaveItem still refuses to create one", func(t *testing.T) {
		store, _ := newTestStore(t)
		err := store.SaveItem(&offlinecache.ItemRecord{Item: dp1playlist.PlaylistItem{Source: ""}})
		require.Error(t, err, "the write path is what keeps this shape unreachable in the first place")
	})
}
