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
// Five rules keep it from serving anything the live path would not:
//
//   - It is restored only while the device is KNOWN to have no network
//     (SetOffline). With network present, or with the answer unknown, the
//     attach arms nothing and scope comes from the next SyncPlaylist, as it
//     did before this existed. Request matching is by method and URL, so a
//     scope restored over a page nobody has confirmed could answer another
//     work's request for a shared script with this capture's old bytes;
//     with no network there is no live answer for it to displace.
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
//   - It ends when internet returns: DropRestoredScope turns replay off
//     unless a sync has replaced the restored scope by then.
//   - It never competes with an authoritative sync: restore runs under the
//     playback lock, gives way when a display path already holds it, and
//     arms nothing once a sync has completed on the attached session.

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

// SetOffline wires the no-network signal restore is gated on. offline must
// answer from memory (no probe, no I/O: it runs on the CDP connect path)
// and return true only when the device is known to have no network; unknown
// is false. Never set, nothing is ever restored. Not on the KioskReplay
// interface (main.go type-asserts it), so its mocks stay as they are.
func (k *kioskReplay) SetOffline(offline func() bool) { k.offline.Store(&offline) }

func (k *kioskReplay) knownOffline() bool {
	offline := k.offline.Load()
	return offline != nil && *offline != nil && (*offline)()
}

// rememberScope records the scope a successful SyncPlaylist just applied,
// and counts that sync (syncs).
// An empty scope removes the file: cleared scope, nothing to restore.
// Best-effort: a failed write costs only the next boot's head start, and
// forgets the memo so the next sync writes again whatever its scope. Both
// the write and the removal are synced to disk, file then directory: these
// devices lose power without warning, and a scope that vanished with the
// page cache would bring back the very failure this exists to fix.
func (k *kioskReplay) rememberScope(cachedSources []string) {
	k.syncs.Add(1)
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
		} else if err == nil {
			err = syncDir(filepath.Dir(path))
		}
	} else {
		err = writeReplayScope(path, keys)
	}
	if err != nil {
		// A *PathError names only the scope file, never a source.
		// The file may hold either scope now (a rename can land and its
		// directory sync fail), so the memo no longer describes it.
		k.logger.Warn("offline cache: failed to remember replay scope for the next start", zap.Error(err))
		k.scopeSaved = false
		return
	}
	k.scopeSaved, k.savedScope = true, joined
}

// writeReplayScope replaces the file through a synced temp file, a rename
// and a directory sync, so a crash or power loss leaves the previous scope
// or this one, never a torn one.
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
	if err := tmp.Sync(); err != nil {
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
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory so a rename or unlink within it is durable.
func syncDir(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: the cache root, parent of the fixed scope file.
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

// restorableSources is what an attach may re-arm: nothing unless the device
// is known to have no network, else the remembered sources still cached.
func (k *kioskReplay) restorableSources() []string {
	if !k.knownOffline() {
		return nil
	}
	return k.rememberedSources()
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
// is where scope came from before this existed. syncsBefore is the sync
// count from just before that attach (dialAndAttach).
func (k *kioskReplay) restoreScope(ctx context.Context, syncsBefore uint64) {
	// An authoritative display path holds the playback lock across its own
	// scope sync and send; it knows what is on screen and this does not.
	// Giving way (rather than waiting) also keeps the CDP connect loop from
	// stalling behind a send to a slow kiosk.
	if !k.playbackMu.TryLock() {
		return
	}
	defer k.playbackMu.Unlock()
	// A sync that finished since the attach left this session its
	// authoritative scope, strict where it should be; do not relax it.
	if k.syncs.Load() != syncsBefore {
		return
	}

	cached := k.restorableSources()
	if len(cached) == 0 {
		return
	}
	k.armed.Store(true)
	// Marked before the call: a failed enable can still leave Fetch on for
	// some targets, and DropRestoredScope has to cover that too.
	k.restored.Store(syncsBefore + 1)
	if err := k.replayer.EnableForPlaylist(ctx, cached, true); err != nil {
		k.logger.Warn("offline cache: failed to restore remembered replay scope on kiosk attach",
			zap.Int("cached_items", len(cached)), zap.Error(err))
		return
	}
	k.logger.Info("offline cache: restored remembered replay scope on kiosk attach",
		zap.Int("cached_items", len(cached)))
	if notify := k.onRestored.Load(); notify != nil && *notify != nil {
		(*notify)()
	}
}

// SetOnScopeRestored registers a callback run once per kiosk attach on which
// the remembered scope was armed, and never when the restore gave way, found
// nothing to arm or failed. main.go uses it to ask the player to load the
// work again: the kiosk's first load ran before this session existed and
// failed, and the kept copy is being served from here on. The callback runs
// on the CDP connect path with the playback lock held, so it must hand its
// work to another goroutine. Not on the KioskReplay interface, like
// SetOffline.
func (k *kioskReplay) SetOnScopeRestored(notify func()) { k.onRestored.Store(&notify) }

// DropRestoredScope turns replay off if the scope in force is still the one
// restoreScope armed. main.go calls it when the internet verdict turns to
// available: the restored scope was justified only by there being no live
// answer to displace, and nothing has confirmed the page it sits over, so
// once origins are reachable it could answer another work's request for a
// shared URL with this capture's old bytes. A scope a SyncPlaylist has
// installed since is authoritative and is left alone; the next sync is what
// scopes replay again, as it did before restore existed. It waits for the
// playback lock, so call it off any path that must not block.
func (k *kioskReplay) DropRestoredScope(ctx context.Context) {
	// Under the lock even to find nothing restored: a restore that read
	// "offline" just before the verdict turned finishes first, and its
	// scope is then dropped here.
	k.playbackMu.Lock()
	defer k.playbackMu.Unlock()
	at := k.restored.Load()
	// Offline again by the time this ran: a restore made since stands.
	if at == 0 || k.knownOffline() {
		return
	}
	if k.syncs.Load()+1 != at {
		k.restored.Store(0)
		return
	}
	if err := k.replayer.Disable(ctx); err != nil {
		// The mark stays, so the next "available" verdict tries again.
		k.logger.Warn("offline cache: failed to drop the restored replay scope when internet returned", zap.Error(err))
		return
	}
	k.restored.Store(0)
	k.logger.Info("offline cache: dropped the restored replay scope, internet is available")
}
