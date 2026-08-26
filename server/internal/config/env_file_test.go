package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/joho/godotenv"
)

func TestUpdateEnvFilePreservesUnknownValuesAndReplacesDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "# retained\nDATABASE_URL=old\nADVANCED_SETTING=keep\nDATABASE_URL=duplicate\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateEnvFile(path, map[string]string{
		"DATABASE_URL":           "postgres://new/value?sslmode=require",
		"SMTP_PASSWORD":          `value with spaces, #, a \\ slash and a " quote`,
		"SYSTEM_SETUP_COMPLETED": "true",
	}); err != nil {
		t.Fatalf("UpdateEnvFile() error = %v", err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	if strings.Count(text, "DATABASE_URL=") != 1 || !strings.Contains(text, "ADVANCED_SETTING=keep") || !strings.Contains(text, "# retained") {
		t.Fatalf("updated file did not preserve or deduplicate values:\n%s", text)
	}
	values, err := godotenv.Read(path)
	if err != nil {
		t.Fatalf("parse updated dotenv: %v", err)
	}
	if values["DATABASE_URL"] != "postgres://new/value?sslmode=require" || values["SMTP_PASSWORD"] != `value with spaces, #, a \\ slash and a " quote` || values["SYSTEM_SETUP_COMPLETED"] != "true" {
		t.Fatalf("parsed values = %#v", values)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestUpdateEnvFileRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires optional Windows privileges")
	}
	directory := t.TempDir()
	target := filepath.Join(directory, "target.env")
	link := filepath.Join(directory, ".env")
	if err := os.WriteFile(target, []byte("VALUE=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := UpdateEnvFile(link, map[string]string{"VALUE": "new"}); err == nil {
		t.Fatal("UpdateEnvFile() accepted a symbolic link")
	}
}
