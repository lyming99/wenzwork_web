//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
)

func fileCreatedAt(info os.FileInfo) time.Time {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return time.Unix(0, data.CreationTime.Nanoseconds()).UTC()
	}
	return info.ModTime().UTC()
}
