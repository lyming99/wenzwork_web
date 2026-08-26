package main

import (
	"errors"
	"os"
	"path/filepath"
)

// preparePrivateConversationDirectory creates the repository directory with
// private permissions and rejects aliases before SQLite is allowed to open a
// database inside it. Keep this separate from the state-file directory: the
// latter may intentionally contain non-private state-adjacent files.
func preparePrivateConversationDirectory(directory string) error {
	directory = filepath.Clean(directory)
	if directory == "." || !filepath.IsAbs(directory) {
		return errConversationInvalid
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errConversationInvalid
	}
	// A Windows directory junction is not consistently marked as a symlink.
	// Resolve it as well so the conversation store cannot be redirected.
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || !sameFilesystemPath(resolved, directory) {
		return errConversationInvalid
	}
	// secureStateFile supplies the protected ACL on Windows. On Unix it first
	// makes the directory 0600, so restore its required search bit afterward.
	if err := secureStateFile(directory); err != nil {
		return err
	}
	return os.Chmod(directory, 0o700)
}

// rejectUnsafeConversationFile prevents SQLite from following a pre-existing
// symlink, opening a non-regular file, or accepting a database that is already
// readable by another local user.
func rejectUnsafeConversationFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return errConversationInvalid
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !sameFilesystemPath(resolved, path) {
		return errConversationInvalid
	}
	if err := verifyStateFileSecurity(path); err != nil {
		return errors.Join(errConversationInvalid, err)
	}
	return nil
}

// secureConversationDatabaseFiles applies the device-agent's private state
// file policy to every SQLite file that can contain conversation content.
func secureConversationDatabaseFiles(path string) error {
	if err := preparePrivateConversationDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	for _, candidate := range []string{path, path + "-journal", path + "-wal", path + "-shm"} {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errConversationInvalid
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || !sameFilesystemPath(resolved, candidate) {
			return errConversationInvalid
		}
		if err := secureStateFile(candidate); err != nil {
			return err
		}
	}
	return nil
}
