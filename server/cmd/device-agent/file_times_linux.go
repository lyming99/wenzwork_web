//go:build linux

package main

import (
	"os"
	"syscall"
	"time"
)

func fileCreatedAt(info os.FileInfo) time.Time {
	// Linux does not expose birth time through os.FileInfo. Match the local
	// workbench's FileStat.changed semantics by reporting inode change time.
	if data, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(data.Ctim.Sec, data.Ctim.Nsec).UTC()
	}
	return info.ModTime().UTC()
}
