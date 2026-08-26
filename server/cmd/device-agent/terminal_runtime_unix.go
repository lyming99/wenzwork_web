//go:build !windows

package main

func platformInteractiveTerminalRuntimeAvailable() bool {
	return true
}
