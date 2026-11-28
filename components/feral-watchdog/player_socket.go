package main

import (
	"fmt"
	"os"
	"syscall"
)

func validatePlayerSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(stat.Uid) != int64(os.Geteuid()) || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("player socket must be a private socket owned by this user")
	}
	return nil
}
