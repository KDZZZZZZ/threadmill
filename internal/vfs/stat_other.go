//go:build !linux

package vfs

import "io/fs"

type changeIdentity struct {
	device      uint64
	inode       uint64
	seconds     int64
	nanoseconds int64
}

func statChangeIdentity(fs.FileInfo) (changeIdentity, bool) {
	return changeIdentity{}, false
}
