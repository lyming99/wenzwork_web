//go:build !linux && !windows

package main

func runAICommandSandboxInternal([]string) (bool, int) {
	return false, 0
}
