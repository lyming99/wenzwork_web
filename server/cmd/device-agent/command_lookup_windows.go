//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// lookupCommandExecutable first searches the active user's reviewed PATH.
// This is intentionally separate from the Agent service PATH: user-installed
// npm CLIs belong to the interactive account and must never be elevated to
// LocalSystem merely to make them discoverable.
func lookupCommandExecutable(name string) (string, error) {
	environment := windowsInteractiveCommandEnvironment()
	if len(environment) > 0 {
		if resolved, err := lookupWindowsExecutableInEnvironment(name, environment); err == nil {
			return resolved, nil
		}
	} else {
		localSystem, identityErr := currentWindowsProcessIsLocalSystem()
		if identityErr != nil {
			return "", errors.Join(errTaskExecutionContextUnavailable, identityErr)
		}
		if localSystem {
			return "", fmt.Errorf("%w: no signed-in Windows user", errTaskExecutionContextUnavailable)
		}
	}
	return exec.LookPath(name)
}

func lookupWindowsExecutableInEnvironment(name string, environment []string) (string, error) {
	pathValue := ""
	pathExtensions := ".COM;.EXE;.BAT;.CMD"
	for _, variable := range environment {
		key, value, found := strings.Cut(variable, "=")
		if !found {
			continue
		}
		switch strings.ToUpper(key) {
		case "PATH":
			pathValue = value
		case "PATHEXT":
			if strings.TrimSpace(value) != "" {
				pathExtensions = value
			}
		}
	}
	if pathValue == "" {
		return "", exec.ErrNotFound
	}
	variants := []string{name}
	if filepath.Ext(name) == "" {
		variants = variants[:0]
		for _, extension := range strings.Split(pathExtensions, ";") {
			extension = strings.TrimSpace(extension)
			if extension == "" {
				continue
			}
			if !strings.HasPrefix(extension, ".") {
				extension = "." + extension
			}
			variants = append(variants, name+extension)
		}
	}
	for _, directory := range filepath.SplitList(pathValue) {
		directory = strings.Trim(strings.TrimSpace(directory), `"`)
		if directory == "" {
			continue
		}
		for _, variant := range variants {
			candidate := filepath.Join(directory, variant)
			info, err := os.Stat(candidate)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			return filepath.Abs(candidate)
		}
	}
	return "", errors.Join(exec.ErrNotFound, os.ErrNotExist)
}
