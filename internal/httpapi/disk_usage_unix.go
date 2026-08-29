//go:build !windows

package httpapi

import (
	"os"
	"syscall"
)

func diskUsage(path string) (total, used, free uint64) {
	if _, err := os.Stat(path); err != nil {
		return 0, 0, 0
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0
	}
	blockSize := uint64(stat.Bsize)
	total = stat.Blocks * blockSize
	free = stat.Bavail * blockSize
	if total >= free {
		used = total - free
	}
	return total, used, free
}
