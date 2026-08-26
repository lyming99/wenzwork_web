package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const maximumEnvironmentFileBytes = 1024 * 1024

// UpdateEnvFile replaces the selected dotenv keys without discarding comments
// or unrelated advanced settings. The replacement is written beside the
// original and atomically renamed so a failed write cannot truncate .env.
func UpdateEnvFile(path string, updates map[string]string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." || strings.ContainsRune(path, '\x00') {
		return errors.New("environment file path is invalid")
	}
	if len(updates) == 0 {
		return errors.New("environment updates are required")
	}
	for key, value := range updates {
		if !validEnvironmentKey(key) {
			return fmt.Errorf("environment key %q is invalid", key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("environment value %s contains a forbidden character", key)
		}
	}

	contents, mode, err := readManagedEnvironmentFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(contents), "\r\n", "\n"), "\n")
	found := make(map[string]bool, len(updates))
	output := make([]string, 0, len(lines)+len(updates)+2)
	for _, line := range lines {
		key, ok := environmentLineKey(line)
		if !ok {
			output = append(output, line)
			continue
		}
		value, selected := updates[key]
		if !selected {
			output = append(output, line)
			continue
		}
		if found[key] {
			continue
		}
		found[key] = true
		output = append(output, key+"="+quoteEnvironmentValue(value))
	}
	missing := make([]string, 0, len(updates))
	for key := range updates {
		if !found[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		if len(output) > 0 && output[len(output)-1] != "" {
			output = append(output, "")
		}
		output = append(output, "# Saved by the WenzWork first-login system setup.")
		for _, key := range missing {
			output = append(output, key+"="+quoteEnvironmentValue(updates[key]))
		}
	}
	for len(output) > 1 && output[len(output)-1] == "" && output[len(output)-2] == "" {
		output = output[:len(output)-1]
	}
	encoded := []byte(strings.Join(output, "\n"))
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		encoded = append(encoded, '\n')
	}
	if len(encoded) > maximumEnvironmentFileBytes {
		return errors.New("updated environment file is unexpectedly large")
	}

	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".wenzwork-env-*")
	if err != nil {
		return fmt.Errorf("create temporary environment file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := true
	defer func() {
		_ = temporary.Close()
		if cleanup {
			_ = os.Remove(temporaryPath)
		}
	}()
	if runtime.GOOS == "windows" {
		mode = 0o600
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return fmt.Errorf("protect temporary environment file: %w", err)
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write temporary environment file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync temporary environment file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary environment file: %w", err)
	}
	if err := replaceEnvironmentFile(temporaryPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// quoteEnvironmentValue emits the small, portable dotenv subset understood
// by godotenv and the packaged Bash/PowerShell lifecycle scripts. Newlines and
// null bytes have already been rejected by UpdateEnvFile.
func quoteEnvironmentValue(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}

func readManagedEnvironmentFile(path string) ([]byte, os.FileMode, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("inspect environment file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, 0, errors.New("environment file must be a regular file")
	}
	if info.Size() > maximumEnvironmentFileBytes {
		return nil, 0, errors.New("environment file is unexpectedly large")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read environment file: %w", err)
	}
	return contents, info.Mode(), nil
}

func replaceEnvironmentFile(source, destination string) error {
	if runtime.GOOS == "windows" {
		backup := destination + ".setup-backup"
		_ = os.Remove(backup)
		if err := os.Rename(destination, backup); err != nil {
			return fmt.Errorf("stage existing environment file: %w", err)
		}
		if err := os.Rename(source, destination); err != nil {
			_ = os.Rename(backup, destination)
			return fmt.Errorf("replace environment file: %w", err)
		}
		if err := os.Remove(backup); err != nil {
			return fmt.Errorf("remove previous environment file backup: %w", err)
		}
		return nil
	}
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("replace environment file: %w", err)
	}
	return nil
}

func environmentLineKey(line string) (string, bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", false
	}
	key, _, ok := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	return key, ok && validEnvironmentKey(key)
}

func validEnvironmentKey(value string) bool {
	if value == "" {
		return false
	}
	scanner := bufio.NewScanner(strings.NewReader(value))
	scanner.Split(bufio.ScanRunes)
	index := 0
	for scanner.Scan() {
		character := scanner.Text()[0]
		if (character < 'A' || character > 'Z') && character != '_' && (index == 0 || character < '0' || character > '9') {
			return false
		}
		index++
	}
	return scanner.Err() == nil
}
