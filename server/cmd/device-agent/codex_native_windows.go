//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maximumNativeCodexScanEntries = 8192

// resolveWindowsNativeCodexExecutable follows the same installation layouts
// as the desktop client: first find the native binary behind the selected npm
// shim, then fall back to a directly discoverable codex.exe. The target is
// launched with argv directly, so cmd.exe never has to reinterpret task
// arguments.
func resolveWindowsNativeCodexExecutable(cliExecutable string) (string, error) {
	if native, err := findWindowsNativeCodexExecutable(cliExecutable); err == nil {
		return native, nil
	}
	if direct, err := resolveSupervisedExecutable("codex.exe"); err == nil {
		return direct, nil
	}
	return "", errors.New("native codex.exe was not found")
}

func findWindowsNativeCodexExecutable(cliExecutable string) (string, error) {
	cliExecutable = strings.TrimSpace(cliExecutable)
	if cliExecutable == "" || !filepath.IsAbs(cliExecutable) {
		return "", errors.New("Codex CLI executable is unavailable")
	}
	info, err := os.Stat(cliExecutable)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("Codex CLI executable is unavailable")
	}
	if strings.EqualFold(filepath.Base(cliExecutable), "codex.exe") {
		return filepath.Abs(cliExecutable)
	}
	roots := []string{
		filepath.Join(filepath.Dir(cliExecutable), "node_modules", "@openai", "codex"),
		filepath.Join(filepath.Dir(cliExecutable), "node_modules", "@openai", "codex", "node_modules"),
	}
	for _, root := range roots {
		native, findErr := findNativeCodexExecutableBelow(root)
		if findErr == nil {
			return native, nil
		}
	}
	return "", errors.New("native codex.exe was not found")
}

func findNativeCodexExecutableBelow(root string) (string, error) {
	seen := 0
	var found string
	errFound := errors.New("native codex executable found")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		seen++
		if seen > maximumNativeCodexScanEntries {
			return fs.SkipAll
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !strings.EqualFold(entry.Name(), "codex.exe") {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		absolute, absoluteErr := filepath.Abs(path)
		if absoluteErr != nil {
			return nil
		}
		found = absolute
		return errFound
	})
	if errors.Is(err, errFound) && found != "" {
		return found, nil
	}
	return "", errors.New("native codex.exe was not found")
}
