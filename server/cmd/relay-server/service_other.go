//go:build !windows

package main

func runAsWindowsServiceIfNeeded() (bool, error) {
	return false, nil
}
