//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// procLocksKey stats path and renders it the way /proc/locks names a file:
// MAJOR:MINOR:INODE, major and minor in two-digit hex, inode in decimal
// (fs/locks.c, lock_get_status).
func procLocksKey(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%02x:%02x:%d", unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino), nil
}
