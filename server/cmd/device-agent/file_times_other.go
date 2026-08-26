//go:build !windows && !linux && !darwin

package main

import (
	"os"
	"time"
)

func fileCreatedAt(info os.FileInfo) time.Time {
	return info.ModTime().UTC()
}
