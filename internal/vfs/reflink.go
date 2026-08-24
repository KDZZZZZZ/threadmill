package vfs

import (
	"syscall"
)

// Linux 文件系统魔数；用于判断 live 目录所在盘是否支持 reflink。
const (
	fsMagicBtrfs = 0x9123683E
	fsMagicXFS   = 0x58465342
)

// ReflinkSupported 报告 root 所在文件系统是否支持 reflink（XFS/btrfs）。
// 在 reflink 文件系统上，Materialize 的 cp --reflink=auto 退化为块级克隆；
// 无法判定时保守返回 false。
func ReflinkSupported(root string) bool {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return false
	}
	magic := uint64(stat.Type)
	return magic == fsMagicBtrfs || magic == fsMagicXFS
}

// ReflinkCloneable 报告从 baseDir 拷贝到 liveRoot 能否走 reflink：
// 要求 liveRoot 在 reflink 文件系统上，且两者在同一设备（跨文件系统内核会回退为全量拷贝）。
func ReflinkCloneable(baseDir, liveRoot string) bool {
	if !ReflinkSupported(liveRoot) {
		return false
	}
	var base, live syscall.Stat_t
	if err := syscall.Stat(baseDir, &base); err != nil {
		return false
	}
	if err := syscall.Stat(liveRoot, &live); err != nil {
		return false
	}
	return base.Dev == live.Dev
}
