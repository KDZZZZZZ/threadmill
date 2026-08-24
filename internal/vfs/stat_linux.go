//go:build linux

package vfs

import (
	"io/fs"
	"syscall"
)

type changeIdentity struct {
	device      uint64
	inode       uint64
	seconds     int64
	nanoseconds int64
}

func statChangeIdentity(info fs.FileInfo) (changeIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return changeIdentity{}, false
	}
	return changeIdentity{
		device:      stat.Dev,
		inode:       stat.Ino,
		seconds:     stat.Ctim.Sec,
		nanoseconds: stat.Ctim.Nsec,
	}, true
}
