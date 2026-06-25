//go:build linux

package config

import "golang.org/x/sys/unix"

func openat2Supported() bool {
	fd, err := unix.Openat2(unix.AT_FDCWD, "/", &unix.OpenHow{})
	if err != nil {
		return false
	}
	_ = unix.Close(fd)
	return true
}
