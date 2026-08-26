//go:build !windows

package main

import "io"

func runPlatformService(arguments []string, stderr io.Writer) (bool, error) {
	return false, nil
}
