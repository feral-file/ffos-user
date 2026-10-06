//go:build !linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// procLocksKey is the non-Linux build of lockkey_linux.go's helper. Devices
// never have /proc/locks; this exists so the package and its fixture-driven
// tests build and run on developer machines (st.Dev is int32 on darwin).
func procLocksKey(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	dev := uint64(st.Dev) //nolint:gosec // device numbers are never negative.
	return fmt.Sprintf("%02x:%02x:%d", unix.Major(dev), unix.Minor(dev), st.Ino), nil
}
