package manager

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/reekoheek/aim/pkg/backend"
)

type testEntry struct {
	name string
	body string
	mode fs.FileMode
	dir  bool
	link string
}

func buildTarGz(t *testing.T, entries []testEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: int64(entry.mode), Size: int64(len(entry.body)), Typeflag: tar.TypeReg}
		switch {
		case entry.dir:
			header.Typeflag = tar.TypeDir
			header.Size = 0
		case entry.link != "":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.link
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func buildZip(t *testing.T, entries []testEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		mode := entry.mode
		body := entry.body
		if entry.link != "" {
			mode |= fs.ModeSymlink
			body = entry.link
		}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestExtract(t *testing.T) {
	tests := []struct {
		name        string
		format      backend.ArchiveFormat
		entries     []testEntry
		stripPrefix string
		wantFiles   map[string]string
		wantModes   map[string]fs.FileMode
		wantLinks   map[string]string
		wantErr     bool
	}{
		{
			name:   "tar.gz with top level folder",
			format: backend.ArchiveTarGz,
			entries: []testEntry{
				{name: "llama-b1/", dir: true, mode: 0o755},
				{name: "llama-b1/llama-server", body: "server", mode: 0o755},
				{name: "llama-b1/libggml.so.0", body: "lib", mode: 0o644},
				{name: "llama-b1/libggml.so", link: "libggml.so.0"},
				{name: "llama-b1/sub/readme", body: "doc", mode: 0o600},
			},
			stripPrefix: "llama-b1/",
			wantFiles:   map[string]string{"llama-server": "server", "libggml.so.0": "lib", "sub/readme": "doc"},
			wantModes:   map[string]fs.FileMode{"llama-server": 0o755, "libggml.so.0": 0o644, "sub/readme": 0o600},
			wantLinks:   map[string]string{"libggml.so": "libggml.so.0"},
		},
		{
			name:        "flat zip with modes",
			format:      backend.ArchiveZip,
			entries:     []testEntry{{name: "sd-server", body: "server", mode: 0o755}, {name: "start-server.bat", body: "bat", mode: 0o644}, {name: "libx.so", link: "libx.so.1"}, {name: "libx.so.1", body: "x", mode: 0o755}},
			stripPrefix: "",
			wantFiles:   map[string]string{"sd-server": "server", "start-server.bat": "bat"},
			wantModes:   map[string]fs.FileMode{"sd-server": 0o755, "start-server.bat": 0o644},
			wantLinks:   map[string]string{"libx.so": "libx.so.1"},
		},
		{
			name:        "flat zip ignores absent strip prefix",
			format:      backend.ArchiveZip,
			entries:     []testEntry{{name: "llama-server.exe", body: "exe", mode: 0o644}},
			stripPrefix: "llama-b1/",
			wantFiles:   map[string]string{"llama-server.exe": "exe"},
		},
		{
			name:      "tar.gz without mode gets default",
			format:    backend.ArchiveTarGz,
			entries:   []testEntry{{name: "file", body: "f"}},
			wantFiles: map[string]string{"file": "f"},
			wantModes: map[string]fs.FileMode{"file": defaultFileMode},
		},
		{
			name:      "path traversal is contained",
			format:    backend.ArchiveTarGz,
			entries:   []testEntry{{name: "../../escape", body: "e", mode: 0o644}},
			wantFiles: map[string]string{"escape": "e"},
		},
		{
			name:    "symlink escaping the folder is rejected",
			format:  backend.ArchiveTarGz,
			entries: []testEntry{{name: "evil", link: "../../etc/passwd"}},
			wantErr: true,
		},
		{
			name:    "absolute symlink is rejected",
			format:  backend.ArchiveZip,
			entries: []testEntry{{name: "evil", link: "/etc/passwd"}},
			wantErr: true,
		},
		{
			name:    "unknown format",
			format:  backend.ArchiveFormat("rar"),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			archivePath := filepath.Join(directory, "archive")
			var content []byte
			if tt.format == backend.ArchiveZip {
				content = buildZip(t, tt.entries)
			} else {
				content = buildTarGz(t, tt.entries)
			}
			if err := os.WriteFile(archivePath, content, 0o644); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(directory, "out")

			err := Extract(archivePath, tt.format, tt.stripPrefix, destination)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Extract() error = %v, wantErr %v", err, tt.wantErr)
			}
			for name, body := range tt.wantFiles {
				got, err := os.ReadFile(filepath.Join(destination, name))
				if err != nil || string(got) != body {
					t.Fatalf("file %s = %q (%v), want %q", name, got, err, body)
				}
			}
			if runtime.GOOS != "windows" {
				for name, mode := range tt.wantModes {
					info, err := os.Stat(filepath.Join(destination, name))
					if err != nil || info.Mode().Perm() != mode {
						t.Fatalf("mode of %s = %v (%v), want %v", name, info.Mode().Perm(), err, mode)
					}
				}
				for name, target := range tt.wantLinks {
					got, err := os.Readlink(filepath.Join(destination, name))
					if err != nil || got != target {
						t.Fatalf("link %s = %q (%v), want %q", name, got, err, target)
					}
				}
			}
		})
	}
}
