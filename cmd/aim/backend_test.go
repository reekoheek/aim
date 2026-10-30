package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reekoheek/aim/internal/manager"
	"github.com/reekoheek/aim/pkg/backend"
)

type memorySource struct {
	releases []backend.Release
}

func (s memorySource) LatestRelease(ctx context.Context, repository string) (backend.Release, error) {
	return s.releases[0], nil
}

func (s memorySource) ReleaseByTag(ctx context.Context, repository string, tag string) (backend.Release, error) {
	for _, release := range s.releases {
		if release.Tag == tag {
			return release, nil
		}
	}
	return backend.Release{}, manager.ErrNotFound
}

func (s memorySource) Releases(ctx context.Context, repository string, perPage int, page int) ([]backend.Release, error) {
	if page > 1 {
		return nil, nil
	}
	return s.releases, nil
}

func (s memorySource) Download(ctx context.Context, address string, destination io.Writer) error {
	return errors.New("downloads are not expected in this test")
}

func TestBackendCmd(t *testing.T) {
	source := memorySource{releases: []backend.Release{
		{Tag: "master-1028-37bc88b", Assets: []backend.Asset{{Name: "sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64.zip", URL: "https://example.test/sd-cpu.zip"}}},
		{Tag: "b12002", Assets: []backend.Asset{{Name: "llama-b12002-bin-ubuntu-vulkan-x64.tar.gz", URL: "https://example.test/llama-vulkan.tar.gz"}}},
	}}

	tests := []struct {
		name       string
		args       []string
		prepare    func(t *testing.T, root string)
		wantErr    bool
		wantOutput []string
	}{
		{
			name:       "lists backends",
			args:       []string{"backend", "--help"},
			wantOutput: []string{"llama.cpp", "sd.cpp"},
		},
		{
			name:       "lists backend subcommands",
			args:       []string{"backend", "llama.cpp", "--help"},
			wantOutput: []string{"ls", "add", "use", "rm"},
		},
		{
			name:    "rejects backend aliases",
			args:    []string{"backend", "llama", "ls"},
			wantErr: true,
		},
		{
			name:       "install dry run prints asset url",
			args:       []string{"backend", "llama.cpp", "add", "b12002", "--dry-run"},
			wantOutput: []string{"https://example.test/llama-vulkan.tar.gz"},
		},
		{
			name:       "install with variant",
			args:       []string{"backend", "sd.cpp", "add", "dev", "--variant", "cpu", "--dry-run"},
			wantOutput: []string{"master-1028-37bc88b-cpu", "https://example.test/sd-cpu.zip"},
		},
		{
			name:       "install help documents stable versions for llama.cpp",
			args:       []string{"backend", "llama.cpp", "add", "--help"},
			wantOutput: []string{"vX.Y.Z", "--variant", "--keep", "--no-use", "--dry-run", "--force"},
		},
		{
			name: "list local reads the root",
			args: []string{"backend", "llama.cpp", "ls", "-l"},
			prepare: func(t *testing.T, root string) {
				folder := filepath.Join(root, "llama-b11951-vulkan")
				if err := os.MkdirAll(folder, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(folder, "llama-server"), []byte("x"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			wantOutput: []string{"llama-b11951-vulkan", "vulkan"},
		},
		{
			name:    "rejects list alias",
			args:    []string{"backend", "llama.cpp", "list"},
			wantErr: true,
		},
		{
			name:    "rejects install alias",
			args:    []string{"backend", "llama.cpp", "install"},
			wantErr: true,
		},
		{
			name:    "rejects unknown subcommand",
			args:    []string{"backend", "sd.cpp", "update"},
			wantErr: true,
		},
		{
			name:       "backend without subcommand prints help",
			args:       []string{"backend"},
			wantOutput: []string{"llama.cpp", "sd.cpp"},
		},
		{
			name:    "rm requires an argument",
			args:    []string{"backend", "sd.cpp", "rm"},
			wantErr: true,
		},
		{
			name:    "use rejects variant flag",
			args:    []string{"backend", "llama.cpp", "use", "b1", "--variant", "rocm"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "root")
			if tt.prepare != nil {
				tt.prepare(t, root)
			}
			dependencies := backendDependencies{
				source:      source,
				environment: manager.Environment{OS: "linux", Home: base, Lookup: func(key string) string { return "" }},
				platform:    backend.Platform{OS: "linux", Arch: "amd64"},
				input:       strings.NewReader(""),
			}
			cmd := newRootCmd(func(cfgFile string) error { return nil }, dependencies)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(append(tt.args, "--root", root, "--bin-dir", filepath.Join(base, "bin")))

			err := cmd.Execute()

			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v\n%s", err, tt.wantErr, output.String())
			}
			for _, text := range tt.wantOutput {
				if !strings.Contains(output.String(), text) {
					t.Fatalf("output does not contain %q:\n%s", text, output.String())
				}
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	tests := []struct {
		name string
		file *os.File
		want bool
	}{
		{name: "regular file", file: file, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminal(tt.file); got != tt.want {
				t.Fatalf("isTerminal() = %v, want %v", got, tt.want)
			}
		})
	}
}
