//go:build !windows

package main

import (
	"errors"
	"os"
)

func verifyStateFileSecurity(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return errors.New("agent state permissions must be 0600")
	}
	return nil
}

func secureStateFile(path string) error {
	return os.Chmod(path, 0o600)
}
