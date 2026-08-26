//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"
)

func fileCreatedAt(info os.FileInfo) time.Time {
	if data, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(data.Birthtimespec.Sec, data.Birthtimespec.Nsec).UTC()
	}
	return info.ModTime().UTC()
}
