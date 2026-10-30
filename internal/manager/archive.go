package manager

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/reekoheek/aim/pkg/backend"
)

const (
	defaultFileMode      fs.FileMode = 0o644
	defaultDirectoryMode fs.FileMode = 0o755
	permissionBits       fs.FileMode = 0o777
)

type archiveEntry struct {
	name       string
	mode       fs.FileMode
	isDir      bool
	linkTarget string
	open       func() (io.ReadCloser, error)
}

func Extract(archivePath string, format backend.ArchiveFormat, stripPrefix string, destination string) error {
	switch format {
	case backend.ArchiveTarGz:
		return extractTarGz(archivePath, stripPrefix, destination)
	case backend.ArchiveZip:
		return extractZip(archivePath, stripPrefix, destination)
	default:
		return fmt.Errorf("unsupported archive format %q", format)
	}
}

func extractTarGz(archivePath string, stripPrefix string, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	decompressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("read gzip: %w", err)
	}
	defer decompressed.Close()
	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		entry := archiveEntry{name: header.Name, mode: fs.FileMode(header.Mode) & permissionBits}
		switch header.Typeflag {
		case tar.TypeDir:
			entry.isDir = true
		case tar.TypeSymlink:
			entry.linkTarget = header.Linkname
		case tar.TypeReg:
			entry.open = func() (io.ReadCloser, error) { return io.NopCloser(reader), nil }
		default:
			continue
		}
		if err := writeEntry(entry, stripPrefix, destination); err != nil {
			return err
		}
	}
}

func extractZip(archivePath string, stripPrefix string, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("read zip: %w", err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		info := file.FileInfo()
		entry := archiveEntry{name: file.Name, mode: info.Mode() & permissionBits, isDir: info.IsDir(), open: file.Open}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := readZipSymlink(file)
			if err != nil {
				return err
			}
			entry.linkTarget = target
			entry.open = nil
		}
		if err := writeEntry(entry, stripPrefix, destination); err != nil {
			return err
		}
	}
	return nil
}

func readZipSymlink(file *zip.File) (string, error) {
	content, err := file.Open()
	if err != nil {
		return "", err
	}
	defer content.Close()
	target, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	return string(target), nil
}

func writeEntry(entry archiveEntry, stripPrefix string, destination string) error {
	name := strings.TrimPrefix(path.Clean("/"+entry.name), "/")
	if stripPrefix != "" {
		prefix := strings.TrimSuffix(stripPrefix, "/")
		if name == prefix {
			return nil
		}
		name = strings.TrimPrefix(name, prefix+"/")
	}
	if name == "" || name == "." {
		return nil
	}
	target := filepath.Join(destination, filepath.FromSlash(name))
	switch {
	case entry.isDir:
		return os.MkdirAll(target, defaultDirectoryMode)
	case entry.linkTarget != "":
		return writeSymlink(entry.linkTarget, target, destination)
	default:
		return writeFile(entry, target)
	}
}

func writeSymlink(linkTarget string, target string, destination string) error {
	resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(linkTarget))
	if filepath.IsAbs(linkTarget) || !isWithin(destination, resolved) {
		return fmt.Errorf("archive symlink %s escapes the build folder", linkTarget)
	}
	if err := os.MkdirAll(filepath.Dir(target), defaultDirectoryMode); err != nil {
		return err
	}
	return os.Symlink(linkTarget, target)
}

func writeFile(entry archiveEntry, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), defaultDirectoryMode); err != nil {
		return err
	}
	mode := entry.mode
	if mode == 0 {
		mode = defaultFileMode
	}
	source, err := entry.open()
	if err != nil {
		return err
	}
	defer source.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, source); err != nil {
		output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

func isWithin(root string, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
