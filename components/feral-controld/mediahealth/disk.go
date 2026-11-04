package mediahealth

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// statfsFree reports the free bytes available to an unprivileged writer on
// the filesystem holding path (Bavail, not Bfree: the root-reserved blocks
// are not ours to fill). Same arithmetic as the offline cache's storage
// policy, so the two reserves are comparable.
func statfsFree(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &stat); err != nil {
		return 0, fmt.Errorf("media health: statfs: %w", err)
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil //nolint:gosec // G115: block counts fit int64 on every supported target.
}
