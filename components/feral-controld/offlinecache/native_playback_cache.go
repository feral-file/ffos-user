package offlinecache

import (
	"context"

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
		if !record.Automatic || record.freshForPlayback(time.Now()) {
			count++
		}
	}
	return count, nil
}
