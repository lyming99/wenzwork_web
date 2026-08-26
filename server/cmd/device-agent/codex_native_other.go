//go:build !windows

package main

import "errors"

func resolveWindowsNativeCodexExecutable(string) (string, error) {
	return "", errors.New("Windows native Codex is unsupported on this platform")
}
