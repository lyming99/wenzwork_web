package relayrelease

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	ManifestSchemaVersion = 1
	maximumManifestBytes  = 1 << 20
	maximumManifestFiles  = 256
	PlatformLinux         = "linux"
	PlatformWindows       = "windows"
	PlatformDarwin        = "darwin"
	ArchitectureAMD64     = "amd64"
	ArchitectureARM64     = "arm64"
)

var (
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	keyIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)
	hexPattern     = regexp.MustCompile(`^[0-9a-f]+$`)
)

type Manifest struct {
	SchemaVersion int            `json:"schemaVersion"`
	Version       string         `json:"version"`
	Platform      string         `json:"platform"`
	Architecture  string         `json:"architecture"`
	ProtocolMin   int            `json:"protocolMin"`
	ProtocolMax   int            `json:"protocolMax"`
	Commit        string         `json:"commit"`
	BuildTimeUnix int64          `json:"buildTimeUnix"`
	SigningKeyID  string         `json:"signingKeyId"`
	Files         []ManifestFile `json:"files"`
}

type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type VerifyOptions struct {
	Root            string
	ManifestPath    string
	Version         string
	Platform        string
	Architecture    string
	ProtocolVersion int
}

// SupportsTarget is the canonical Relay host support matrix. Each operating
// system and CPU architecture pair has its own binary, service integration,
// and signed manifest.
func SupportsTarget(platform, architecture string) bool {
	switch platform {
	case PlatformLinux, PlatformWindows, PlatformDarwin:
		return architecture == ArchitectureAMD64 || architecture == ArchitectureARM64
	default:
		return false
	}
}

// Verify checks the signed bundle's semantic contract after the outer archive
// signature has been verified. It rejects unlisted files and all links so a
// release cannot smuggle content outside the version directory.
func Verify(options VerifyOptions) (Manifest, error) {
	root, err := filepath.Abs(filepath.Clean(strings.TrimSpace(options.Root)))
	if err != nil || strings.TrimSpace(options.Root) == "" {
		return Manifest{}, errors.New("Relay release root is invalid")
	}
	manifestPath := filepath.Clean(strings.TrimSpace(options.ManifestPath))
	if manifestPath == "." || manifestPath == "" {
		manifestPath = filepath.Join(root, "release-manifest.json")
	} else if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(root, manifestPath)
	}
	manifestPath, err = filepath.Abs(manifestPath)
	if err != nil || !withinRoot(root, manifestPath) {
		return Manifest{}, errors.New("Relay release manifest path escapes the package root")
	}

	file, err := os.Open(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("open Relay release manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumManifestBytes {
		return Manifest{}, errors.New("Relay release manifest size or file type is invalid")
	}
	var manifest Manifest
	decoder := json.NewDecoder(io.LimitReader(file, maximumManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode Relay release manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("Relay release manifest contains trailing data")
	}
	if err := validateManifest(manifest, options); err != nil {
		return Manifest{}, err
	}

	listed := make(map[string]ManifestFile, len(manifest.Files))
	previous := ""
	for _, entry := range manifest.Files {
		path, err := validateManifestPath(root, entry.Path)
		if err != nil {
			return Manifest{}, err
		}
		if entry.Path <= previous {
			return Manifest{}, errors.New("Relay release manifest file paths must be unique and sorted")
		}
		previous = entry.Path
		if entry.Size < 0 || len(entry.SHA256) != 64 || !hexPattern.MatchString(entry.SHA256) {
			return Manifest{}, fmt.Errorf("Relay release manifest metadata is invalid for %q", entry.Path)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			return Manifest{}, fmt.Errorf("Relay release file metadata does not match manifest: %q", entry.Path)
		}
		digest, err := digestFile(path)
		if err != nil {
			return Manifest{}, err
		}
		if digest != entry.SHA256 {
			return Manifest{}, fmt.Errorf("Relay release file digest does not match manifest: %q", entry.Path)
		}
		listed[filepath.ToSlash(entry.Path)] = entry
	}

	var actual []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("Relay release package contains an unsupported file type: %q", path)
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if samePath(path, manifestPath) {
			return nil
		}
		actual = append(actual, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return Manifest{}, fmt.Errorf("inspect Relay release package: %w", err)
	}
	slices.Sort(actual)
	if len(actual) != len(listed) {
		return Manifest{}, errors.New("Relay release package contains missing or unlisted files")
	}
	for _, path := range actual {
		if _, ok := listed[path]; !ok {
			return Manifest{}, fmt.Errorf("Relay release package contains an unlisted file: %q", path)
		}
	}
	return manifest, nil
}

func validateManifest(manifest Manifest, options VerifyOptions) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || !versionPattern.MatchString(manifest.Version) ||
		!SupportsTarget(manifest.Platform, manifest.Architecture) ||
		manifest.ProtocolMin < 1 || manifest.ProtocolMax < manifest.ProtocolMin ||
		len(manifest.Commit) < 40 || len(manifest.Commit) > 64 || !hexPattern.MatchString(manifest.Commit) ||
		manifest.BuildTimeUnix <= 0 || !keyIDPattern.MatchString(manifest.SigningKeyID) ||
		len(manifest.Files) == 0 || len(manifest.Files) > maximumManifestFiles {
		return errors.New("Relay release manifest contract is invalid")
	}
	if options.Version != "" && manifest.Version != options.Version {
		return errors.New("Relay release version does not match the requested version")
	}
	if options.Platform != "" && manifest.Platform != options.Platform {
		return errors.New("Relay release platform does not match this host")
	}
	if options.Architecture != "" && manifest.Architecture != options.Architecture {
		return errors.New("Relay release architecture does not match this host")
	}
	if options.ProtocolVersion > 0 && (options.ProtocolVersion < manifest.ProtocolMin || options.ProtocolVersion > manifest.ProtocolMax) {
		return errors.New("Relay release protocol is incompatible with this host")
	}
	return nil
}

func validateManifestPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || strings.Contains(relative, "\\") || filepath.ToSlash(relative) != relative ||
		strings.ContainsAny(relative, "\x00\r\n") || relative != filepath.ToSlash(filepath.Clean(relative)) || relative == "." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("Relay release manifest contains an unsafe path: %q", relative)
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	if !withinRoot(root, path) {
		return "", fmt.Errorf("Relay release manifest path escapes the package root: %q", relative)
	}
	return path, nil
}

func withinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func samePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open Relay release file: %w", err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash Relay release file: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
