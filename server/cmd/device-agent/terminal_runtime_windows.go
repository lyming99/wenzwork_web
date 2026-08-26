//go:build windows

package main

import "github.com/Kodecable/crosspty"

func platformInteractiveTerminalRuntimeAvailable() bool {
	if !crosspty.IsConPTYSupported() {
		return false
	}
	_, err := windowsSupervisedPTYDesktopName()
	return err == nil
}
