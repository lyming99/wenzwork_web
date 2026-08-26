package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	directoryMode  = 0o755
	regularMode    = 0o644
	executableMode = 0o755
)

var normalizedTimestamp = time.Unix(0, 0).UTC()

func main() {
	if len(os.Args) != 4 || (os.Args[1] != "create" && os.Args[1] != "verify") {
		fmt.Fprintln(os.Stderr, "usage: deployment_archive <create SOURCE|verify ARCHIVE> TARGET")
		os.Exit(2)
	}
	var err error
	if os.Args[1] == "create" {
		err = createArchive(os.Args[2], os.Args[3])
	} else {
		if os.Args[2] != "-" {
			err = errors.New("verify requires '-' as its unused SOURCE argument")
		} else {
			err = verifyArchive(os.Args[3])
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func createArchive(source, destination string) (returnErr error) {
	root, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("archive source is not a directory: %s", root)
	}

	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() {
		if closeErr := output.Close(); returnErr == nil && closeErr != nil {
			returnErr = closeErr
		}
		if returnErr != nil {
			_ = os.Remove(destination)
		}
	}()

	gzipWriter := gzip.NewWriter(output)
	gzipWriter.Header.ModTime = normalizedTimestamp
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	closeWriters := func() error {
		if err := tarWriter.Close(); err != nil {
			return err
		}
		return gzipWriter.Close()
	}

	files := 0
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if err := validatePath(relative); err != nil {
			return err
		}
		if relative == ".env" {
			return errors.New("deployment archive source must not contain root .env")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("archive source contains a link or special file: %s", relative)
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = "./" + relative
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.ModTime = normalizedTimestamp
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Format = tar.FormatPAX
		if info.IsDir() {
			header.Name += "/"
			header.Mode = directoryMode
			header.Typeflag = tar.TypeDir
			header.Size = 0
		} else {
			header.Mode = int64(expectedFileMode(relative))
			header.Typeflag = tar.TypeReg
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		input, err := os.Open(current)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		files++
		return nil
	})
	if err != nil {
		_ = tarWriter.Close()
		_ = gzipWriter.Close()
		return err
	}
	if files == 0 {
		_ = tarWriter.Close()
		_ = gzipWriter.Close()
		return errors.New("archive source contains no files")
	}
	if err := closeWriters(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	return nil
}

func verifyArchive(archivePath string) error {
	input, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer input.Close()
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	seen := make(map[string]struct{})
	files := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		name := strings.TrimSuffix(strings.TrimPrefix(header.Name, "./"), "/")
		if name == "" {
			continue
		}
		if err := validatePath(name); err != nil {
			return err
		}
		if name == ".env" {
			return errors.New("deployment archive must not contain root .env")
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("archive contains duplicate path: %s", name)
		}
		seen[name] = struct{}{}

		switch header.Typeflag {
		case tar.TypeDir:
			if fs.FileMode(header.Mode).Perm() != directoryMode {
				return fmt.Errorf("directory mode for %s is %04o, expected %04o", name, header.Mode&0o777, directoryMode)
			}
		case tar.TypeReg, tar.TypeRegA:
			expected := expectedFileMode(name)
			if fs.FileMode(header.Mode).Perm() != expected {
				return fmt.Errorf("file mode for %s is %04o, expected %04o", name, header.Mode&0o777, expected)
			}
			files++
		default:
			return fmt.Errorf("archive contains a link or special entry: %s", name)
		}
	}
	if files == 0 {
		return errors.New("archive contains no files")
	}
	return nil
}

func expectedFileMode(name string) fs.FileMode {
	name = path.Clean(strings.TrimPrefix(name, "./"))
	extension := strings.ToLower(path.Ext(name))
	bootstrapVerifier := strings.HasPrefix(name, "config/relay-bootstrap/") && strings.HasPrefix(path.Base(name), "relayctl-")
	if strings.HasPrefix(name, "bin/") || bootstrapVerifier || extension == ".sh" || extension == ".ps1" {
		return executableMode
	}
	return regularMode
}

func validatePath(name string) error {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return fmt.Errorf("unsafe archive path: %q", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != name {
		return fmt.Errorf("unsafe archive path: %q", name)
	}
	return nil
}
