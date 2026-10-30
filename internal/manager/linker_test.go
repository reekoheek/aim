package manager

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/reekoheek/aim/pkg/backend"
)

func TestLinkerRelink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bin links are not used on windows")
	}

	tests := []struct {
		name        string
		prepare     func(t *testing.T, root string, otherRoot string, binDir string)
		wantLinked  []string
		wantRemoved []string
		wantSkipped []string
		wantLinks   map[string]string
		wantFiles   []string
		wantMissing []string
	}{
		{
			name:       "links allow-listed binaries only",
			wantLinked: []string{"llama-server", "llama-cli"},
			wantLinks:  map[string]string{"llama-server": "ROOT/current/llama-server", "llama-cli": "ROOT/current/llama-cli"},
			wantMissing: []string{
				"start-server.sh",
				"libggml-base.so",
			},
		},
		{
			name: "never overwrites a real file",
			prepare: func(t *testing.T, root string, otherRoot string, binDir string) {
				writeTestFile(t, filepath.Join(binDir, "llama-server"), 0o755)
			},
			wantLinked:  []string{"llama-cli"},
			wantSkipped: []string{"llama-server"},
			wantFiles:   []string{"llama-server"},
		},
		{
			name: "replaces an existing link",
			prepare: func(t *testing.T, root string, otherRoot string, binDir string) {
				symlink(t, filepath.Join(root, "old", "llama-server"), filepath.Join(binDir, "llama-server"))
			},
			wantLinked: []string{"llama-server", "llama-cli"},
			wantLinks:  map[string]string{"llama-server": "ROOT/current/llama-server"},
		},
		{
			name: "removes stale links into the same root",
			prepare: func(t *testing.T, root string, otherRoot string, binDir string) {
				symlink(t, filepath.Join(root, "current", "llama-gone"), filepath.Join(binDir, "llama-gone"))
				symlink(t, "../root/current/llama-relative", filepath.Join(binDir, "llama-relative"))
			},
			wantLinked:  []string{"llama-server", "llama-cli"},
			wantRemoved: []string{"llama-gone", "llama-relative"},
			wantMissing: []string{"llama-gone", "llama-relative"},
		},
		{
			name: "keeps links into other roots and real files",
			prepare: func(t *testing.T, root string, otherRoot string, binDir string) {
				symlink(t, filepath.Join(otherRoot, "current", "sd-server"), filepath.Join(binDir, "sd-server"))
				writeTestFile(t, filepath.Join(binDir, "my-script"), 0o755)
			},
			wantLinked: []string{"llama-server", "llama-cli"},
			wantLinks:  map[string]string{"sd-server": "OTHER/current/sd-server"},
			wantFiles:  []string{"my-script"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "root")
			otherRoot := filepath.Join(base, "rootx")
			binDir := filepath.Join(base, "bin")
			buildPath := filepath.Join(root, "b1-vulkan")
			for _, name := range []string{"llama-server", "llama-cli", "start-server.sh", "libggml-base.so"} {
				writeTestFile(t, filepath.Join(buildPath, name), 0o755)
			}
			writeTestFile(t, filepath.Join(buildPath, "llama-bench"), 0o644)
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.prepare != nil {
				tt.prepare(t, root, otherRoot, binDir)
			}
			descriptor := backend.LlamaCpp
			descriptor.LinkedBinaries = []string{"llama-server", "llama-cli", "llama-bench", "llama-missing"}
			linker := Linker{Descriptor: descriptor, Root: root, BinDir: binDir}

			report, err := linker.Relink(buildPath)

			if err != nil {
				t.Fatalf("Relink() error = %v", err)
			}
			if !reflect.DeepEqual(report.Linked, tt.wantLinked) || !reflect.DeepEqual(report.Removed, tt.wantRemoved) || !reflect.DeepEqual(report.Skipped, tt.wantSkipped) {
				t.Fatalf("Relink() = %+v, want linked %v removed %v skipped %v", report, tt.wantLinked, tt.wantRemoved, tt.wantSkipped)
			}
			for name, want := range tt.wantLinks {
				want = filepath.FromSlash(want)
				want = filepath.Join(replacePrefix(want, "ROOT", root, "OTHER", otherRoot))
				got, err := os.Readlink(filepath.Join(binDir, name))
				if err != nil || got != want {
					t.Fatalf("link %s = %q (%v), want %q", name, got, err, want)
				}
			}
			for _, name := range tt.wantFiles {
				info, err := os.Lstat(filepath.Join(binDir, name))
				if err != nil || !info.Mode().IsRegular() {
					t.Fatalf("%s is not a regular file anymore", name)
				}
			}
			for _, name := range tt.wantMissing {
				if _, err := os.Lstat(filepath.Join(binDir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s exists in bin dir", name)
				}
			}
		})
	}
}

func replacePrefix(path string, firstMarker string, firstValue string, secondMarker string, secondValue string) string {
	switch {
	case len(path) >= len(firstMarker) && path[:len(firstMarker)] == firstMarker:
		return firstValue + path[len(firstMarker):]
	case len(path) >= len(secondMarker) && path[:len(secondMarker)] == secondMarker:
		return secondValue + path[len(secondMarker):]
	default:
		return path
	}
}

func writeTestFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target string, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
