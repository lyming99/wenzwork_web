//go:build !windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func taskLogFileHasSingleLink(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	return ok && stat.Nlink == 1
}

func taskLogDiskFreeBytes(path string) (uint64, error) {
	path, err := existingTaskLogProbePath(path)
	if err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	blocks, blockSize := uint64(stat.Bavail), uint64(stat.Bsize)
	if blockSize != 0 && blocks > ^uint64(0)/blockSize {
		return ^uint64(0), nil
	}
	return blocks * blockSize, nil
}

func syncTaskLogDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	return errors.Join(err, directory.Close())
}
