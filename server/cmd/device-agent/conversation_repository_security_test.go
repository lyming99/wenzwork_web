package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestConversationStorageHelpersSecureDirectoryAndDatabaseFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "conversations.chat")
	if err := preparePrivateConversationDirectory(directory); err != nil {
		t.Fatalf("prepare private directory: %v", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatalf("stat private directory: %v", err)
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		t.Fatalf("private directory has unsafe mode: %v", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory permissions = %o, want 0700", info.Mode().Perm())
	}

	database := filepath.Join(directory, conversationDatabaseFilename)
	for _, path := range []string{database, database + "-journal", database + "-wal", database + "-shm"} {
		if err := os.WriteFile(path, []byte("conversation"), 0o600); err != nil {
			t.Fatalf("create %q: %v", filepath.Base(path), err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("relax %q permissions: %v", filepath.Base(path), err)
		}
	}
	if err := secureConversationDatabaseFiles(database); err != nil {
		t.Fatalf("secure database files: %v", err)
	}
	for _, path := range []string{database, database + "-journal", database + "-wal", database + "-shm"} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", filepath.Base(path), err)
		}
		if !info.Mode().IsRegular() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			t.Fatalf("database file %q has unsafe mode: %v", filepath.Base(path), info.Mode())
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("database file %q permissions = %o, want 0600", filepath.Base(path), info.Mode().Perm())
		}
	}
}

func TestRejectUnsafeConversationFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.sqlite3")
	if err := os.WriteFile(target, []byte("conversation"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias.sqlite3")
	if err := os.Symlink(target, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a test symlink is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := rejectUnsafeConversationFile(alias); !errors.Is(err, errConversationInvalid) {
		t.Fatalf("symlink rejection = %v, want conversation validation error", err)
	}
}

func TestRejectUnsafeConversationFileRejectsNonPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not authoritative on Windows")
	}
	path := filepath.Join(t.TempDir(), "conversations.sqlite3")
	if err := os.WriteFile(path, []byte("conversation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnsafeConversationFile(path); !errors.Is(err, errConversationInvalid) {
		t.Fatalf("non-private file rejection = %v, want conversation validation error", err)
	}
}
