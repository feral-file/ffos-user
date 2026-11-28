package offlinecache

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// nativePlaybackCache adapts the existing cast admission guard to per-occurrence
// native replay. It never installs browser interception or owns playback state.
// The legacy AttachOnReconnect hook is inert here. Remove that hook/interface
// when the final diagnostic kiosk replay consumer is retired; normal playback
// already has no CDP endpoint or shared interception scope.
type nativePlaybackCache struct {
	store      Store
	mu         sync.Mutex
	generation atomic.Uint64
}

func NewNativePlaybackCache(store Store) KioskReplay                   { return &nativePlaybackCache{store: store} }
func (n *nativePlaybackCache) AttachOnReconnect(context.Context) error { return nil }
func (n *nativePlaybackCache) LockPlayback()                           { n.mu.Lock() }
func (n *nativePlaybackCache) UnlockPlayback()                         { n.mu.Unlock() }
func (n *nativePlaybackCache) PlaybackGeneration() uint64              { return n.generation.Load() }
func (n *nativePlaybackCache) MarkPlaybackChanged()                    { n.generation.Add(1) }
func (n *nativePlaybackCache) SyncPlaylist(ctx context.Context, sources []string) (int, error) {
	seen := map[string]bool{}
	count := 0
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		if seen[source] {
			continue
		}
		if len(seen) >= maxRescueLookups {
			break
		}
		seen[source] = true
		record, err := n.store.LoadItem(SourceKey(source))
		if err != nil {
			continue
		}
		if record.freshForPlayback(time.Now()) && nativeReplayRecordReady(record) {
			count++
		}
	}
	return count, nil
}

func nativeReplayRecordReady(record *ItemRecord) bool {
	var entry *Resource
	for i := range record.Resources {
		r := &record.Resources[i]
		if r.EffectiveMethod() == "GET" && (r.URL == record.Item.Source || r.URL == record.Entry) {
			entry = r
			break
		}
	}
	if entry == nil || !entry.freshAt(time.Now()) {
		return false
	}
	mime := strings.ToLower(entry.ProbeContentType())
	if (strings.HasPrefix(mime, "image/") && !strings.HasPrefix(mime, "image/svg")) || strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/") {
		return true
	}
	for _, r := range record.Resources {
		// Unsupported writes are coverage misses. The player never replays them.
		if safeIdempotentMethods[r.EffectiveMethod()] && r.BrowserPolicyVersion != 2 {
			return false
		}
		if !r.freshAt(time.Now()) {
			return false
		}
	}
	return entry.BrowserPolicyVersion == 2
}
