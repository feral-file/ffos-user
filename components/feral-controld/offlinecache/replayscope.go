package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// The remembered replay scope: which cached sources replay was serving when
// SyncPlaylist last succeeded, kept on disk so AttachOnReconnect can re-arm
// it before anything has asked the player what it is showing. That is what
// lets a device booted with no network give the kiosk's first artwork
// request the kept copy (feral-file/feral-file#3635) — see
// AttachOnReconnect's doc for why the next SyncPlaylist is too late.
//
// Three rules keep it from serving anything the live path would not:
//
//   - The file is a hint, the store is the truth. It holds source KEYS
//     (SourceKey), never URLs, and restore re-reads every record through the
//     same scopeFor a live sync uses. A cleared item's record is gone, so it
//     is never restored — across a reboot too — whether or not the clear's
//     own resync ran. An unreadable, corrupt or missing file restores
//     nothing and is never an error.
//   - A restored scope is always MIXED: a miss passes through to the network
//     even under fail_closed. Nothing has confirmed what the reloaded page
//     shows, and a strict scope over the wrong playlist would fail requests
//     the page could have made. The cached sources themselves are served
//     from the cache, exactly as they are once the playlist's own sync runs.
//   - It never competes with an authoritative sync: restore runs under the
//     playback lock and gives way when a display path already holds it.

// replayScopeFileName sits at the cache root, beside items/ and blobs/, so
// it lives and dies with the records it names.
const replayScopeFileName = "replay-scope.json"

// replayScopeMaxBytes bounds what restore will read. A key is 64 hex
// characters; 4 MiB is tens of thousands of them, far past any scope a
// playlist produces, and keeps a damaged file from costing startup memory.
const replayScopeMaxBytes = 4 << 20

type replayScopeFile struct {
	SourceKeys []string `json:"sourceKeys"`
}

func (k *kioskReplay) scopePath() string {
	root := k.store.RootDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, replayScopeFileName)
}

// rememberScope records the scope a successful SyncPlaylist just applied.
// An empty scope removes the file: cleared scope, nothing to restore.
// Best-effort: a failed write costs only the next boot's head start.
func (k *kioskReplay) rememberScope(cachedSources []string) {
	path := k.scopePath()
	if path == "" {
		return
	}
	keys := make([]string, len(cachedSources))
	for i, source := range cachedSources {
		keys[i] = SourceKey(source)
	}
	joined := strings.Join(keys, ",")

	k.scopeMu.Lock()
	defer k.scopeMu.Unlock()
	if k.scopeSaved && k.savedScope == joined {
		return
	}
	var err error
	if len(keys) == 0 {
		if err = os.Remove(path); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	} else {
		err = writeReplayScope(path, keys)
	}
	if err != nil {
		// A *PathError names only the scope file, never a source.
		k.logger.Warn("offline cache: failed to remember replay scope for the next start", zap.Error(err))
		return
	}
	k.scopeSaved, k.savedScope = true, joined
}

// writeReplayScope replaces the file through a temp file and rename, so a
// crash mid-write leaves the previous scope or none, never a torn one.
func writeReplayScope(path string, keys []string) error {
	data, err := json.Marshal(replayScopeFile{SourceKeys: keys})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), replayScopeFileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// rememberedSources returns the remembered sources replay can still serve
// right now: each key is resolved to its record's own source and then put
// through scopeFor, so a cleared, corrupt or expired-automatic record drops
// out here.
func (k *kioskReplay) rememberedSources() []string {
	path := k.scopePath()
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > replayScopeMaxBytes {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the fixed scope file name under the cache root.
	if err != nil {
		return nil
	}
	var file replayScopeFile
	if err := json.Unmarshal(data, &file); err != nil {
		k.logger.Warn("offline cache: ignoring unreadable remembered replay scope", zap.Error(err))
		return nil
	}
	sources := make([]string, 0, len(file.SourceKeys))
	for _, key := range file.SourceKeys {
		// LoadItem refuses anything that is not a well-formed key and
		// verifies the record against it, so a hand-edited file cannot
		// point replay at a path or at another source's record.
		rec, err := k.store.LoadItem(key)
		if err != nil {
			continue
		}
		sources = append(sources, rec.Item.Source)
	}
	cached, _ := k.scopeFor(sources)
	return cached
}

// restoreScope re-arms the remembered scope on the session AttachOnReconnect
// just attached. Failure is logged and left to the next SyncPlaylist, which
// is where scope came from before this existed.
func (k *kioskReplay) restoreScope(ctx context.Context) {
	// An authoritative display path holds the playback lock across its own
	// scope sync and send; it knows what is on screen and this does not.
	// Giving way (rather than waiting) also keeps the CDP connect loop from
	// stalling behind a send to a slow kiosk.
	if !k.playbackMu.TryLock() {
		return
	}
	defer k.playbackMu.Unlock()

	cached := k.rememberedSources()
	if len(cached) == 0 {
		return
	}
	k.armed.Store(true)
	if err := k.replayer.EnableForPlaylist(ctx, cached, true); err != nil {
		k.logger.Warn("offline cache: failed to restore remembered replay scope on kiosk attach",
			zap.Int("cached_items", len(cached)), zap.Error(err))
		return
	}
	k.logger.Info("offline cache: restored remembered replay scope on kiosk attach",
		zap.Int("cached_items", len(cached)))
}
