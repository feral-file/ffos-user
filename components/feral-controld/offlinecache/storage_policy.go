package offlinecache

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
)

var errCacheDiskPressure = errors.New("artwork cache: storage reserve reached")

type storagePolicy struct {
	root              string
	percent           int
	minFree, maxBytes int64
	space             func(string) (total, available int64, err error)
}

func (p *storagePolicy) reserve(total int64) int64 { return max(p.minFree, total/10) }

func (p *storagePolicy) limit(total, available, used int64) int64 {
	limit := total/100*int64(p.percent) + total%100*int64(p.percent)/100
	if p.maxBytes > 0 {
		limit = min(limit, p.maxBytes)
	}
	return max(0, min(limit, used+available-p.reserve(total)))
}

func (p *storagePolicy) currentLimit(used int64) (int64, error) {
	total, available, err := p.space(p.root)
	if err != nil {
		return 0, err
	}
	return p.limit(total, available, used), nil
}

// Before the first capture the cache directory may not exist. Its nearest
// existing parent is on the filesystem that will hold the new directory.
func filesystemSpace(path string) (int64, int64, error) {
	for {
		var stat syscall.Statfs_t
		err := syscall.Statfs(path, &stat)
		if err == nil {
			// Statfs_t.Bsize is int64 on linux but uint32 on darwin; widen it
			// once so the arithmetic below compiles on both.
			bsize := int64(stat.Bsize)
			if bsize <= 0 || stat.Blocks > uint64(math.MaxInt64)/uint64(bsize) || stat.Bavail > stat.Blocks {
				return 0, 0, errors.New("artwork cache: invalid filesystem capacity")
			}
			// The positive block size and division bound above make both products fit int64.
			return int64(stat.Blocks) * bsize, int64(stat.Bavail) * bsize, nil //nolint:gosec // G115: capacity is bounded before conversion.
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(path) == path {
			return 0, 0, err
		}
		path = filepath.Dir(path)
	}
}

// One store, one budget for manual and automatic captures. Guard at the
// streaming boundary too: space can disappear after a job was admitted.
type budgetStore struct {
	Store
	policy *storagePolicy
}

func (s *budgetStore) WriteBlob(r io.Reader, maxBytes int64) (string, error) {
	used, err := s.DiskUsage()
	if err != nil {
		return "", err
	}
	limit, err := s.policy.currentLimit(used)
	if err != nil {
		return "", err
	}
	// Leave room for the record and filesystem bookkeeping after the blob.
	room := limit - used - 64*1024
	if room <= 0 {
		return "", errCacheDiskPressure
	}
	if maxBytes <= 0 || room < maxBytes {
		maxBytes = room
	}
	return s.Store.WriteBlob(&spaceGuardReader{reader: r, policy: s.policy}, maxBytes)
}

type spaceGuardReader struct {
	reader io.Reader
	policy *storagePolicy
}

func (r *spaceGuardReader) Read(b []byte) (int, error) {
	total, available, err := r.policy.space(r.policy.root)
	if err != nil {
		return 0, err
	}
	room := available - r.policy.reserve(total) - 64*1024
	if room <= 0 {
		return 0, errCacheDiskPressure
	}
	if int64(len(b)) > room {
		b = b[:room]
	}
	return r.reader.Read(b)
}

func (s *service) diskBudget() (int64, error) {
	if s.storage == nil {
		return s.maxDiskBytes, nil
	}
	used, err := s.store.DiskUsage()
	if err != nil {
		return 0, err
	}
	return s.storage.currentLimit(used)
}
