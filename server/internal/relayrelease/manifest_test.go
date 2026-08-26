package relayrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyReleaseManifestAndFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "VERSION", []byte("v1.2.3\n"))
	writeTestFile(t, root, "bin/relayctl", []byte("signed binary"))
	writeTestManifest(t, root, []string{"VERSION", "bin/relayctl"})

	manifest, err := Verify(VerifyOptions{
		Root: root, Version: "v1.2.3", Platform: "linux", Architecture: "amd64", ProtocolVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SigningKeyID != "relay-test-2026" {
		t.Fatalf("SigningKeyID = %q", manifest.SigningKeyID)
	}
}

func TestVerifyReleaseManifestSupportsTargetMatrix(t *testing.T) {
	for _, platform := range []string{PlatformLinux, PlatformWindows, PlatformDarwin} {
		for _, architecture := range []string{ArchitectureAMD64, ArchitectureARM64} {
			t.Run(platform+"/"+architecture, func(t *testing.T) {
				root := t.TempDir()
				writeTestFile(t, root, "VERSION", []byte("v1.2.3\n"))
				contents, err := os.ReadFile(filepath.Join(root, "VERSION"))
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(contents)
				manifest := testManifest([]ManifestFile{{
					Path: "VERSION", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(contents)),
				}})
				manifest.Platform = platform
				manifest.Architecture = architecture
				writeManifest(t, root, manifest)

				if _, err := Verify(VerifyOptions{
					Root: root, Version: "v1.2.3", Platform: platform,
					Architecture: architecture, ProtocolVersion: 1,
				}); err != nil {
					t.Fatalf("Verify() error = %v", err)
				}
			})
		}
	}
}

func TestSupportsTargetRejectsUnsupportedHosts(t *testing.T) {
	tests := []struct {
		platform     string
		architecture string
		want         bool
	}{
		{PlatformLinux, ArchitectureAMD64, true},
		{PlatformLinux, ArchitectureARM64, true},
		{PlatformWindows, ArchitectureAMD64, true},
		{PlatformWindows, ArchitectureARM64, true},
		{PlatformDarwin, ArchitectureAMD64, true},
		{PlatformDarwin, ArchitectureARM64, true},
		{PlatformLinux, "386", false},
		{"freebsd", ArchitectureAMD64, false},
	}
	for _, test := range tests {
		if got := SupportsTarget(test.platform, test.architecture); got != test.want {
			t.Fatalf("SupportsTarget(%q, %q) = %t, want %t", test.platform, test.architecture, got, test.want)
		}
	}
}

func TestVerifyRejectsTamperingUnlistedFilesAndUnsafePaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "tampered", mutate: func(t *testing.T, root string) {
			writeTestFile(t, root, "VERSION", []byte("tampered\n"))
		}},
		{name: "unlisted", mutate: func(t *testing.T, root string) {
			writeTestFile(t, root, "secret", []byte("not listed"))
		}},
		{name: "path traversal", mutate: func(t *testing.T, root string) {
			manifest := testManifest([]ManifestFile{{Path: "../outside", SHA256: strings.Repeat("0", 64), Size: 0}})
			writeManifest(t, root, manifest)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, root, "VERSION", []byte("v1.2.3\n"))
			writeTestManifest(t, root, []string{"VERSION"})
			test.mutate(t, root)
			if _, err := Verify(VerifyOptions{Root: root, Version: "v1.2.3", Platform: "linux", Architecture: "amd64", ProtocolVersion: 1}); err == nil {
				t.Fatal("Verify() succeeded for an unsafe package")
			}
		})
	}
}

func TestVerifyRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "VERSION", []byte("v1.2.3\n"))
	writeTestManifest(t, root, []string{"VERSION"})
	if err := os.Symlink(filepath.Join(root, "VERSION"), filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := Verify(VerifyOptions{Root: root, Version: "v1.2.3", Platform: "linux", Architecture: "amd64", ProtocolVersion: 1}); err == nil {
		t.Fatal("Verify() accepted a symlink")
	}
}

func writeTestManifest(t *testing.T, root string, paths []string) {
	t.Helper()
	files := make([]ManifestFile, 0, len(paths))
	for _, relative := range paths {
		contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		files = append(files, ManifestFile{Path: relative, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(contents))})
	}
	writeManifest(t, root, testManifest(files))
}

func testManifest(files []ManifestFile) Manifest {
	return Manifest{
		SchemaVersion: 1, Version: "v1.2.3", Platform: "linux", Architecture: "amd64",
		ProtocolMin: 1, ProtocolMax: 1, Commit: "0123456789abcdef0123456789abcdef01234567",
		BuildTimeUnix: 1786000000, SigningKeyID: "relay-test-2026", Files: files,
	}
}

func writeManifest(t *testing.T, root string, manifest Manifest) {
	t.Helper()
	contents, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "release-manifest.json", contents)
}

func writeTestFile(t *testing.T, root, relative string, contents []byte) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
}
