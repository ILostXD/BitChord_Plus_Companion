package main

import (
	"os"
	"syscall"
)

func diskSpace(path string) (*storageSpace, error) {
	var disk syscall.Statfs_t
	if err := syscall.Statfs(path, &disk); err != nil {
		return nil, err
	}
	return &storageSpace{disk.Blocks * uint64(disk.Bsize), disk.Bavail * uint64(disk.Bsize)}, nil
}

func sameDisk(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	first, ok := a.Sys().(*syscall.Stat_t)
	second, otherOK := b.Sys().(*syscall.Stat_t)
	return ok && otherOK && first.Dev == second.Dev
}
