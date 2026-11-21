package main

import (
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func lastUse(info os.FileInfo) time.Time {
	at := info.Sys().(*syscall.Stat_t).Atim
	return time.Unix(at.Sec, at.Nsec)
}
func validKey(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func available(root string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return 0, err
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	return int64(stat.Bavail)*int64(stat.Bsize) - max(int64(5<<30), total/10), nil
}

type entry struct {
	path             string
	used             time.Time
	size             int64
	browser, expired bool
}

func browserEntry(path string, now time.Time) (entry, error) {
	info, err := os.Stat(path)
	if err != nil {
		return entry{}, err
	}
	e := entry{path: path, used: info.ModTime(), browser: true}
	err = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			i, err := d.Info()
			if err != nil {
				return err
			}
			e.size += i.Size()
		}
		return nil
	})
	if now.Sub(e.used) >= time.Hour {
		e.tryBrowser(now, false)
	}
	return e, err
}
func (e entry) tryBrowser(now time.Time, remove bool) bool {
	d, err := os.OpenFile(e.path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer d.Close()
	if err = syscall.Flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK && now.Sub(e.used) >= time.Hour {
			_ = os.Chtimes(e.path, now, now)
		}
		return false
	}
	i, err := d.Stat()
	return remove && err == nil && i.ModTime().Equal(e.used) && os.RemoveAll(e.path) == nil
}

// Both the launcher and this sweeper lock each browser directory. An active
// renderer can retain bytes above a newly reduced budget until it retires.
func prune(root string, r request, reserve int64, now time.Time) (bool, error) {
	var entries []entry
	var used int64
	dirs, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, d := range dirs {
		if !d.IsDir() || !validKey(d.Name()) {
			continue
		}
		e, err := browserEntry(filepath.Join(root, d.Name()), now)
		if err != nil {
			return false, err
		}
		used += e.size
		entries = append(entries, e)
	}
	media := filepath.Join(root, "media")
	files, err := os.ReadDir(media)
	if err != nil {
		return false, err
	}
	for _, f := range files {
		if !f.Type().IsRegular() || !validKey(f.Name()) {
			continue
		}
		i, err := f.Info()
		if err != nil {
			return false, err
		}
		e := entry{path: filepath.Join(media, f.Name()), used: lastUse(i), size: i.Size(), expired: !i.ModTime().After(now)}
		entries = append(entries, e)
		used += e.size
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.expired != b.expired {
			return a.expired
		}
		return a.used.Before(b.used)
	})
	free, err := available(root)
	if err != nil {
		return false, err
	}
	limit := min(r.MaxBytes, used+free) - reserve
	cutoff := now.AddDate(0, 0, -r.MaxAgeDays)
	for _, e := range entries {
		if !e.expired && e.used.After(cutoff) && used <= limit {
			break
		}
		if e.browser {
			if !e.tryBrowser(now, true) {
				continue
			}
		} else if os.Remove(e.path) != nil {
			continue
		}
		used -= e.size
	}
	return used <= limit, nil
}
