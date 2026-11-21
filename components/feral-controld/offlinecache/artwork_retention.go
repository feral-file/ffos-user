package offlinecache

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// The native player's global policy is replaced by atomic rename.
type artworkRetention struct {
	MaxAgeDays int   `json:"maxAgeDays"`
	MaxBytes   int64 `json:"maxBytes"`
}

func artworkPaths() (policy, browser string) {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".state/player/global-policy.json"), filepath.Join(home, ".cache/player-artworks")
}

func readArtworkRetention() artworkRetention {
	defaults := artworkRetention{30, 1 << 30}
	path, _ := artworkPaths()
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) // #nosec G304 -- fixed player policy path, final symlinks refused
	if err != nil {
		return defaults
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return defaults
	}
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	settings := struct {
		Cache artworkRetention `json:"cache"`
	}{defaults}
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &settings) != nil {
		return defaults
	}
	if settings.Cache.MaxAgeDays < 1 || settings.Cache.MaxAgeDays > 36500 || settings.Cache.MaxBytes < 1 || settings.Cache.MaxBytes > 1<<50 {
		return defaults
	}
	return settings.Cache
}

type retentionEntry struct {
	key, source, directory string
	used                   time.Time
	bytes                  int64
	blobs                  []string
}

// Never remove a live Chromium cache. Browser launch takes the same directory
// lock without waiting and falls back to its disposable profile on contention.
func maintainBrowserCache(path string, observed, now time.Time, prune bool) bool {
	dir, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- validated source-key directory under the cache root
	if err != nil {
		return false
	}
	defer func() { _ = dir.Close() }()
	if err := syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK && now.Sub(observed) >= time.Hour {
			_ = os.Chtimes(path, now, now)
		}
		return false
	}
	if !prune {
		return false
	}
	stat, err := dir.Stat()
	if err != nil || !stat.ModTime().Equal(observed) {
		return false
	}
	return os.RemoveAll(path) == nil
}

func (s *service) pruneArtworkRetention(policy artworkRetention, browser string) {
	var entries []retentionEntry
	var used int64
	now := s.clock.Now()
	dirs, _ := os.ReadDir(browser)
	for _, dir := range dirs {
		if !dir.IsDir() || !validSourceKey(dir.Name()) {
			continue
		}
		path := filepath.Join(browser, dir.Name())
		stat, err := dir.Info()
		if err != nil {
			continue
		}
		entry := retentionEntry{directory: path, used: stat.ModTime()}
		// A running browser may hold this directory for weeks and is killed
		// as a process group on retirement, so teardown cannot mark last use.
		if now.Sub(entry.used) >= time.Hour {
			maintainBrowserCache(path, entry.used, now, false)
		}
		err = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() {
				entry.bytes += info.Size()
			}
			return err
		})
		if err == nil {
			used += entry.bytes
			entries = append(entries, entry)
		}
	}
	refs := map[string]int{}
	sizes := map[string]int64{}
	current := ""
	if s.automatic != nil {
		current = s.automatic.currentKey()
	}
	keys, _ := s.store.ListItemKeys()
	for _, key := range keys {
		rec, err := s.store.LoadItem(key)
		if err != nil || !rec.Automatic {
			continue
		}
		entry := retentionEntry{key: key, source: rec.Item.Source, used: rec.LastPlayedAt}
		if entry.used.IsZero() {
			entry.used = rec.CapturedAt
		}
		// A single work can remain on screen for months without a playlist
		// transition. Persist its use at most hourly.
		if key == current && rec.freshForPlayback(now) {
			if now.Sub(entry.used) >= time.Hour {
				_ = s.store.TouchItem(key, now)
			}
			entry.used = now
		}
		for _, r := range rec.Resources {
			if r.SHA256 != "" {
				if refs[r.SHA256] == 0 {
					size, err := s.store.BlobSize(r.SHA256)
					if err != nil {
						continue
					}
					sizes[r.SHA256] = size
					used += size
				}
				refs[r.SHA256]++
				entry.blobs = append(entry.blobs, r.SHA256)
			}
		}
		entries = append(entries, entry)
	}
	limit := policy.MaxBytes
	if s.storage != nil {
		total, available, err := s.storage.space(s.storage.root)
		if err == nil {
			limit = min(limit, max(0, used+available-s.storage.reserve(total)))
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].used.Before(entries[j].used) })
	cutoff := s.clock.Now().AddDate(0, 0, -policy.MaxAgeDays)
	removedMedia := false
	for _, entry := range entries {
		if used <= limit && entry.used.After(cutoff) {
			break
		}
		if entry.directory != "" {
			if !maintainBrowserCache(entry.directory, entry.used, now, true) {
				continue
			}
		} else {
			removed, err := s.store.DeleteItem(entry.key)
			if err != nil || !removed {
				continue
			}
			for _, hash := range entry.blobs {
				refs[hash]--
				if refs[hash] == 0 {
					used -= sizes[hash]
				}
			}
			removedMedia = true
			s.notifyEvicted(entry.source, Coverage{})
		}
		used -= entry.bytes
	}
	if removedMedia {
		if _, _, err := s.gc(); err != nil {
			s.logger.Warn("artwork cache: collecting evicted blobs", zap.Error(err))
		}
	}
}
