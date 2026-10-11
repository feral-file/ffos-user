package offlinecache

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"
)

const maxAutomaticItems = 2048
const automaticRetryInterval = 5 * time.Minute

// playbackCache is fed by player status, not cast intent: scheduled and
// restored playlists therefore warm the same cache. Only the service's one
// capture worker calls prepare; Observe performs no network or disk I/O.
type playbackCache struct {
	s            *service
	mu           sync.Mutex
	sources      []string
	index        int
	ready        bool
	generation   uint64
	clearBarrier uint64
	attempts     map[string]time.Time
	touched      string
}

func (p *playbackCache) currentKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sources) == 0 {
		return ""
	}
	return SourceKey(p.sources[p.index])
}

func (p *playbackCache) observe(items []dp1playlist.PlaylistItem, index int, ready bool) {
	if len(items) > maxAutomaticItems || index < 0 || index >= len(items) {
		items = nil
		index = 0
	}
	sources := make([]string, len(items))
	for i, item := range items {
		if len(item.Source) > MaxSourceURLBytes {
			sources = nil
			index = 0
			break
		}
		sources[i] = item.Source
	}
	p.mu.Lock()
	changed := !slices.Equal(p.sources, sources)
	wake := changed || p.index != index || p.ready != ready
	if changed {
		p.sources = sources
		p.generation++
		p.attempts = make(map[string]time.Time)
		p.clearBarrier = p.s.ClearBarrier()
		p.touched = ""
	}
	p.index, p.ready = index, ready
	p.mu.Unlock()
	if wake {
		select {
		case p.s.queue.wake <- struct{}{}:
		default:
		}
	}
	if changed && p.s.onCacheChanged != nil {
		p.s.onCacheChanged()
	}
}

func (p *playbackCache) prepare(ctx context.Context) {
	p.mu.Lock()
	if !p.ready || len(p.sources) == 0 {
		p.mu.Unlock()
		return
	}
	current := p.sources[p.index]
	touch := current != p.touched
	p.touched = current
	generation, barrier := p.generation, p.clearBarrier
	sources := append(slices.Clone(p.sources[p.index:]), p.sources[:p.index]...)
	p.mu.Unlock()
	now := p.s.clock.Now()
	if touch {
		if err := p.s.store.TouchItem(SourceKey(current), now); err != nil && !errors.Is(err, ErrItemNotFound) {
			p.s.logger.Warn("automatic cache: recording playback failed", zap.Error(err))
		}
	}
	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		if source == "" || p.s.ClearedSinceBarrier("", barrier, source) {
			continue
		}
		key := SourceKey(source)
		if rec, err := p.s.store.LoadItem(key); err == nil && rec.freshForPlayback(now) {
			continue
		}
		p.mu.Lock()
		if p.generation != generation {
			p.mu.Unlock()
			return
		}
		attempted, seen := p.attempts[key]
		if seen && now.Sub(attempted) < automaticRetryInterval {
			p.mu.Unlock()
			continue
		}
		p.attempts[key] = now
		p.mu.Unlock()
		// One probe per pass; never a goroutine per artwork or an unbounded
		// playlist download. Live playback remains in the kiosk process.
		epoch := p.s.currentEpoch(key)
		probeCtx, cancel := context.WithTimeout(ctx, classifyItemTimeout)
		class, err := p.s.classifier.Classify(probeCtx, source)
		cancel()
		if err != nil || class != ClassMedia {
			return
		}
		p.mu.Lock()
		stillCurrent := p.generation == generation
		p.mu.Unlock()
		if !stillCurrent || p.s.ClearedSinceBarrier("", barrier, source) {
			return
		}
		_, err = p.s.enqueueCapture(dp1playlist.PlaylistItem{Source: source}, epoch, class, true)
		if err != nil {
			p.s.logger.Debug("automatic cache: enqueue deferred", zap.Error(err))
		}
		return
	}
}
