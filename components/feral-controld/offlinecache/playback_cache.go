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
	attempts     map[string]automaticAttempt
	touched      string
}

type automaticAttempt struct {
	at      time.Time
	cached  bool
	blocked bool
}

// Keep the active playlist resident during speculative warming. The hard
// storage ceiling can still evict it when unrelated writers consume space.
func (p *playbackCache) protectedKeys() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make(map[string]bool, len(p.sources))
	for _, source := range p.sources {
		keys[SourceKey(source)] = true
	}
	return keys
}

// Expiry discards unusable bytes, not a working-set eviction. Keep the
// source eligible for a fresh capture after backoff, unless policy refused it.
func (p *playbackCache) expiredEvicted(source string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := SourceKey(source)
	if attempt, ok := p.attempts[key]; ok {
		attempt.cached = false
		p.attempts[key] = attempt
	}
}

func (p *playbackCache) completed(source string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := SourceKey(source)
	attempt, exists := p.attempts[key]
	if !exists {
		return
	}
	if err == nil {
		attempt.cached = true
	}
	if errors.Is(err, errAutomaticUncacheable) || errors.Is(err, errCacheDiskPressure) || errors.Is(err, ErrBlobTooLarge) {
		attempt.blocked = true
	}
	p.attempts[key] = attempt
}

func (p *playbackCache) currentKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || len(p.sources) == 0 {
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
	// Missing status suspends work and invalidates an in-flight probe, but
	// does not start a new session or forget a clear/refusal/eviction.
	if len(sources) == 0 {
		p.ready = false
		p.generation++
		p.mu.Unlock()
		return
	}
	changed := !slices.Equal(p.sources, sources)
	wake := changed || p.index != index || p.ready != ready
	if changed {
		p.sources = sources
		p.generation++
		p.attempts = make(map[string]automaticAttempt)
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
		// An expired automatic entry is not what rendered; the network was.
		rec, err := p.s.store.LoadItem(SourceKey(current))
		if err == nil && rec.freshForPlayback(now) {
			err = p.s.store.TouchItem(SourceKey(current), now)
		}
		if err != nil && !errors.Is(err, ErrItemNotFound) {
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
		rec, loadErr := p.s.store.LoadItem(key)
		if loadErr == nil && rec.freshForPlayback(now) {
			p.mu.Lock()
			if p.generation == generation {
				attempt := p.attempts[key]
				attempt.cached = true
				p.attempts[key] = attempt
			}
			p.mu.Unlock()
			continue
		}
		p.mu.Lock()
		if p.generation != generation {
			p.mu.Unlock()
			return
		}
		attempted, seen := p.attempts[key]
		if attempted.blocked || (attempted.cached && loadErr != nil) || (seen && now.Sub(attempted.at) < automaticRetryInterval) {
			p.mu.Unlock()
			continue
		}
		attempted.at = now
		p.attempts[key] = attempted
		p.mu.Unlock()
		// One probe per pass; never a goroutine per artwork or an unbounded
		// playlist download. Live playback remains in the kiosk process.
		epoch := p.s.currentEpoch(key)
		probeCtx, cancel := context.WithTimeout(ctx, classifyItemTimeout)
		class, err := p.s.classifier.Classify(probeCtx, source)
		cancel()
		if err != nil || class != ClassMedia {
			if err == nil {
				p.completed(source, errAutomaticUncacheable)
			}
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
