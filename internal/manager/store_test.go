package manager

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/reekoheek/aim/pkg/backend"
)

var linuxPlatform = backend.Platform{OS: "linux", Arch: "amd64"}

func newTestStore(t *testing.T, descriptor backend.Descriptor) Store {
	t.Helper()
	return Store{Descriptor: descriptor, Root: t.TempDir(), Platform: linuxPlatform, UseSymlinks: runtime.GOOS != "windows"}
}

func addBuild(t *testing.T, store Store, folder string, stamp *Stamp) {
	t.Helper()
	path := filepath.Join(store.Root, folder)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, store.Descriptor.MainBinary), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "llama-cli"), []byte("cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if stamp != nil {
		if err := WriteStamp(path, *stamp); err != nil {
			t.Fatal(err)
		}
	}
}

func folders(builds []Build) []string {
	var names []string
	for _, build := range builds {
		names = append(names, build.Folder)
	}
	return names
}

func TestStoreBuilds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bits are not tracked on windows")
	}
	store := newTestStore(t, backend.LlamaCpp)
	addBuild(t, store, "b100-vulkan", nil)
	addBuild(t, store, "llama-b300-rocm", &Stamp{Stable: "v1.0.0"})
	addBuild(t, store, "b200-vulkan", nil)
	addBuild(t, store, "hand-made", nil)
	addBuild(t, store, ".staging-123", nil)
	if err := os.MkdirAll(filepath.Join(store.Root, "empty-folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, "b400-vulkan"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	nonExecutable := filepath.Join(store.Root, "b500-cpu")
	if err := os.MkdirAll(nonExecutable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonExecutable, "llama-server"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	builds, err := store.Builds()
	if err != nil {
		t.Fatalf("Builds() error = %v", err)
	}

	want := []string{"llama-b300-rocm", "b200-vulkan", "b100-vulkan", "hand-made"}
	if got := folders(builds); !reflect.DeepEqual(got, want) {
		t.Fatalf("Builds() = %v, want %v", got, want)
	}
	first := builds[0]
	if first.Index != 1 || first.Build != "b300" || first.Variant != "rocm" || first.Stable != "v1.0.0" || !first.Managed {
		t.Fatalf("Builds()[0] = %+v", first)
	}
	if builds[3].Managed || builds[3].Index != 4 {
		t.Fatalf("hand-made build = %+v, want unmanaged index 4", builds[3])
	}
}

func TestStoreBuildsMissingRoot(t *testing.T) {
	store := Store{Descriptor: backend.LlamaCpp, Root: filepath.Join(t.TempDir(), "missing")}

	builds, err := store.Builds()

	if err != nil || builds != nil {
		t.Fatalf("Builds() = (%v, %v), want (nil, nil)", builds, err)
	}
}

func TestStoreCurrentAndActivate(t *testing.T) {
	tests := []struct {
		name        string
		useSymlinks bool
		prepare     func(t *testing.T, store Store)
		activate    string
		want        string
		wantErr     bool
	}{
		{name: "nothing active", want: ""},
		{name: "symlink activation", useSymlinks: true, activate: "b200-vulkan", want: "b200-vulkan"},
		{name: "marker activation", useSymlinks: false, activate: "b200-vulkan", want: "b200-vulkan"},
		{
			name:        "absolute llama-manager link",
			useSymlinks: true,
			prepare: func(t *testing.T, store Store) {
				if err := os.Symlink(filepath.Join(store.Root, "llama-b100-vulkan"), filepath.Join(store.Root, CurrentName)); err != nil {
					t.Fatal(err)
				}
			},
			want: "llama-b100-vulkan",
		},
		{
			name:        "swap replaces existing link",
			useSymlinks: true,
			prepare: func(t *testing.T, store Store) {
				if err := store.Activate("b100-vulkan"); err != nil {
					t.Fatal(err)
				}
			},
			activate: "b200-vulkan",
			want:     "b200-vulkan",
		},
		{
			name: "real directory refuses",
			prepare: func(t *testing.T, store Store) {
				if err := os.MkdirAll(filepath.Join(store.Root, CurrentName), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			activate: "b200-vulkan",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.useSymlinks && runtime.GOOS == "windows" {
				t.Skip("symlinks need privileges on windows")
			}
			store := newTestStore(t, backend.LlamaCpp)
			store.UseSymlinks = tt.useSymlinks
			addBuild(t, store, "b100-vulkan", nil)
			addBuild(t, store, "b200-vulkan", nil)
			if tt.prepare != nil {
				tt.prepare(t, store)
			}
			if tt.activate != "" {
				err := store.Activate(tt.activate)
				if (err != nil) != tt.wantErr {
					t.Fatalf("Activate() error = %v, wantErr %v", err, tt.wantErr)
				}
				if tt.wantErr {
					return
				}
				if _, err := os.Lstat(filepath.Join(store.Root, pendingCurrentName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("pending current left behind: %v", err)
				}
				if _, err := os.Stat(filepath.Join(store.Root, CurrentName, store.Descriptor.MainBinary)); tt.useSymlinks && err != nil {
					t.Fatalf("current does not resolve to the build: %v", err)
				}
			}
			got, err := store.Current()
			if err != nil {
				t.Fatalf("Current() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Current() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStoreFind(t *testing.T) {
	store := newTestStore(t, backend.LlamaCpp)
	addBuild(t, store, "b300-vulkan", &Stamp{Stable: "v1.0.0"})
	addBuild(t, store, "b300-rocm", &Stamp{Stable: "v1.0.0"})
	addBuild(t, store, "b200-vulkan", nil)
	addBuild(t, store, "my-custom", nil)
	builds, err := store.Builds()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		argument      string
		want          string
		wantAmbiguous []string
		wantErr       bool
	}{
		{name: "folder name", argument: "b300-rocm", want: "b300-rocm"},
		{name: "index", argument: "3", want: "b200-vulkan"},
		{name: "index out of range", argument: "9", wantErr: true},
		{name: "build tag", argument: "b200", want: "b200-vulkan"},
		{name: "bare build number", argument: "200", want: "b200-vulkan"},
		{name: "ambiguous build", argument: "b300", wantAmbiguous: []string{"b300-vulkan", "b300-rocm"}},
		{name: "ambiguous stable", argument: "v1.0.0", wantAmbiguous: []string{"b300-vulkan", "b300-rocm"}},
		{name: "substring", argument: "custom", want: "my-custom"},
		{name: "no match", argument: "b999", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.Find(tt.argument, builds)
			if tt.wantAmbiguous != nil {
				var ambiguous *AmbiguousBuildError
				if !errors.As(err, &ambiguous) {
					t.Fatalf("Find() error = %v, want AmbiguousBuildError", err)
				}
				gotFolders := folders(ambiguous.Matches)
				sort.Strings(gotFolders)
				wantFolders := append([]string(nil), tt.wantAmbiguous...)
				sort.Strings(wantFolders)
				if !reflect.DeepEqual(gotFolders, wantFolders) {
					t.Fatalf("ambiguous matches = %v, want %v", gotFolders, wantFolders)
				}
				for _, match := range ambiguous.Matches {
					if match.Index == 0 {
						t.Fatalf("ambiguous match %s has no index", match.Folder)
					}
				}
				return
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("Find() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.Folder != tt.want {
				t.Fatalf("Find() = %q, want %q", got.Folder, tt.want)
			}
		})
	}
}

func TestStorePrune(t *testing.T) {
	tests := []struct {
		name       string
		folders    []string
		current    string
		variant    string
		keep       int
		protected  string
		wantPruned []string
	}{
		{
			name:       "keeps newest of the variant",
			folders:    []string{"b500-vulkan", "b400-vulkan", "b300-vulkan", "b200-vulkan", "b100-rocm"},
			current:    "b500-vulkan",
			variant:    "vulkan",
			keep:       2,
			protected:  "b500-vulkan",
			wantPruned: []string{"b300-vulkan", "b200-vulkan"},
		},
		{
			name:       "never prunes active build",
			folders:    []string{"b500-vulkan", "b400-vulkan", "b300-vulkan"},
			current:    "b300-vulkan",
			variant:    "vulkan",
			keep:       1,
			protected:  "b500-vulkan",
			wantPruned: []string{"b400-vulkan"},
		},
		{
			name:       "never prunes protected install",
			folders:    []string{"b500-vulkan", "b400-vulkan", "b100-vulkan"},
			variant:    "vulkan",
			keep:       1,
			protected:  "b100-vulkan",
			wantPruned: []string{"b400-vulkan"},
		},
		{
			name:      "never prunes hand placed folders",
			folders:   []string{"b500-vulkan", "custom-vulkan-build"},
			variant:   "vulkan",
			keep:      1,
			protected: "b500-vulkan",
		},
		{
			name:    "keep zero disables pruning",
			folders: []string{"b500-vulkan", "b400-vulkan"},
			variant: "vulkan",
			keep:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, backend.LlamaCpp)
			store.UseSymlinks = false
			for _, folder := range tt.folders {
				addBuild(t, store, folder, nil)
			}
			if tt.current != "" {
				if err := store.Activate(tt.current); err != nil {
					t.Fatal(err)
				}
			}

			pruned, err := store.Prune(tt.variant, tt.keep, tt.protected)

			if err != nil {
				t.Fatalf("Prune() error = %v", err)
			}
			if !reflect.DeepEqual(pruned, tt.wantPruned) {
				t.Fatalf("Prune() = %v, want %v", pruned, tt.wantPruned)
			}
			for _, folder := range tt.wantPruned {
				if _, err := os.Stat(filepath.Join(store.Root, folder)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s still exists", folder)
				}
			}
		})
	}
}

func TestStoreBuildsSortsUnmanagedByModTime(t *testing.T) {
	store := newTestStore(t, backend.LlamaCpp)
	addBuild(t, store, "older", nil)
	addBuild(t, store, "newer", nil)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(store.Root, "older"), past, past); err != nil {
		t.Fatal(err)
	}

	builds, err := store.Builds()

	if err != nil {
		t.Fatal(err)
	}
	if got := folders(builds); !reflect.DeepEqual(got, []string{"newer", "older"}) {
		t.Fatalf("Builds() = %v", got)
	}
}

func TestDirectorySize(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "a"), make([]byte, 10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "sub", "b"), make([]byte, 5), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want int64
	}{
		{name: "nested files", path: directory, want: 15},
		{name: "missing", path: filepath.Join(directory, "missing"), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DirectorySize(tt.path); got != tt.want {
				t.Fatalf("DirectorySize() = %d, want %d", got, tt.want)
			}
		})
	}
}
