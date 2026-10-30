package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reekoheek/aim/pkg/backend"
)

func llamaArchive(t *testing.T, build string) []byte {
	return buildTarGz(t, []testEntry{
		{name: "llama-" + build + "/", dir: true, mode: 0o755},
		{name: "llama-" + build + "/llama-server", body: build, mode: 0o755},
		{name: "llama-" + build + "/llama-cli", body: build, mode: 0o755},
		{name: "llama-" + build + "/libggml-base.so", body: "lib", mode: 0o755},
	})
}

func llamaInstallReleases(t *testing.T) []fakeRelease {
	vulkan := func(build string) map[string][]byte {
		return map[string][]byte{"llama-" + build + "-bin-ubuntu-vulkan-x64.tar.gz": llamaArchive(t, build)}
	}
	withRocm := vulkan("b400")
	withRocm["llama-b400-bin-ubuntu-rocm-7.0-x64.tar.gz"] = llamaArchive(t, "b400")
	return []fakeRelease{
		{tag: "b500", assets: vulkan("b500")},
		{tag: "v2.0.0", assets: map[string][]byte{"nightly-tag.txt": []byte("b500")}},
		{tag: "b400", assets: withRocm},
		{tag: "b300", assets: vulkan("b300")},
		{tag: "b200", assets: vulkan("b200")},
		{tag: "b100", assets: map[string][]byte{"llama-b100-bin-ubuntu-x64.tar.gz": llamaArchive(t, "b100")}},
		{tag: "b050", assets: map[string][]byte{"llama-b050-bin-ubuntu-vulkan-x64.tar.gz": []byte("not an archive")}},
		{tag: "b040", assets: map[string][]byte{"llama-b040-bin-ubuntu-vulkan-x64.tar.gz": buildTarGz(t, []testEntry{{name: "llama-b040/readme", body: "x", mode: 0o644}})}},
	}
}

type managerFixture struct {
	manager *Manager
	fake    *fakeGitHub
	output  *bytes.Buffer
	root    string
	binDir  string
}

func newManagerFixture(t *testing.T, input string) managerFixture {
	t.Helper()
	fake := newFakeGitHub(t, "v2.0.0", llamaInstallReleases(t))
	base := t.TempDir()
	root := filepath.Join(base, "llama.cpp")
	binDir := filepath.Join(base, "bin")
	descriptor := withRepository(backend.LlamaCpp)
	manager := New(descriptor, fake.client(), linuxPlatform, root, binDir)
	manager.Store.UseSymlinks = runtime.GOOS != "windows"
	output := &bytes.Buffer{}
	manager.Output = output
	manager.Input = strings.NewReader(input)
	return managerFixture{manager: manager, fake: fake, output: output, root: root, binDir: binDir}
}

func (f managerFixture) install(t *testing.T, options InstallOptions) {
	t.Helper()
	if err := f.manager.Install(context.Background(), options); err != nil {
		t.Fatalf("Install(%+v) error = %v\n%s", options, err, f.output)
	}
}

func (f managerFixture) current(t *testing.T) string {
	t.Helper()
	current, err := f.manager.Store.Current()
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func (f managerFixture) exists(folder string) bool {
	_, err := os.Stat(filepath.Join(f.root, folder))
	return err == nil
}

func TestManagerInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses linux assets and symlinks")
	}

	tests := []struct {
		name          string
		before        []InstallOptions
		options       InstallOptions
		wantErr       string
		wantCurrent   string
		wantFolders   []string
		wantGone      []string
		wantOutput    []string
		wantNoOutput  []string
		wantDownloads int
	}{
		{
			name:        "latest stable resolves through nightly-tag",
			options:     InstallOptions{Keep: DefaultKeep},
			wantCurrent: "b500-vulkan",
			wantFolders: []string{"b500-vulkan"},
			wantOutput:  []string{"v2.0.0 -> b500", "current -> b500-vulkan", "no restart needed"},
		},
		{
			name:         "dry run prints url only",
			options:      InstallOptions{Version: "b300", DryRun: true, Keep: DefaultKeep},
			wantGone:     []string{"b300-vulkan"},
			wantOutput:   []string{"dry-run", "/download/b300/llama-b300-bin-ubuntu-vulkan-x64.tar.gz"},
			wantNoOutput: []string{"downloading"},
		},
		{
			name:        "keeps variant of the active build",
			before:      []InstallOptions{{Version: "b400", Variant: "rocm", Keep: DefaultKeep}},
			options:     InstallOptions{Version: "b400", Keep: DefaultKeep},
			wantCurrent: "b400-rocm",
			wantOutput:  []string{"already on b400-rocm"},
		},
		{
			name:        "no-use leaves current alone",
			before:      []InstallOptions{{Version: "b300", Keep: DefaultKeep}},
			options:     InstallOptions{Version: "b200", NoUse: true, Keep: DefaultKeep},
			wantCurrent: "b300-vulkan",
			wantFolders: []string{"b200-vulkan", "b300-vulkan"},
			wantOutput:  []string{"not activated"},
		},
		{
			name:        "prunes older builds of the same variant",
			before:      []InstallOptions{{Version: "b200", Keep: 2}, {Version: "b300", Keep: 2}, {Version: "b100", Variant: "cpu", Keep: 2}},
			options:     InstallOptions{Version: "b400", Variant: "vulkan", Keep: 2},
			wantCurrent: "b400-vulkan",
			wantFolders: []string{"b400-vulkan", "b300-vulkan", "b100-cpu"},
			wantGone:    []string{"b200-vulkan"},
			wantOutput:  []string{"pruned b200-vulkan"},
		},
		{
			name:          "already installed activates without download",
			before:        []InstallOptions{{Version: "b200", NoUse: true, Keep: DefaultKeep}},
			options:       InstallOptions{Version: "b200", Keep: DefaultKeep},
			wantCurrent:   "b200-vulkan",
			wantOutput:    []string{"already downloaded"},
			wantDownloads: 1,
		},
		{
			name:          "force re-downloads",
			before:        []InstallOptions{{Version: "b200", Keep: DefaultKeep}},
			options:       InstallOptions{Version: "b200", Force: true, Keep: DefaultKeep},
			wantCurrent:   "b200-vulkan",
			wantDownloads: 2,
		},
		{
			name:    "missing variant lists builds that have it",
			options: InstallOptions{Version: "b500", Variant: "rocm", Keep: DefaultKeep},
			wantErr: "newest builds that do: b400",
		},
		{
			name:    "unknown variant",
			options: InstallOptions{Variant: "opencl", Keep: DefaultKeep},
			wantErr: `unknown llama.cpp variant "opencl"`,
		},
		{
			name:    "invalid version",
			options: InstallOptions{Version: "banana", Keep: DefaultKeep},
			wantErr: "not a llama.cpp version",
		},
		{
			name:    "corrupt archive",
			options: InstallOptions{Version: "b050", Keep: DefaultKeep},
			wantErr: "extract",
		},
		{
			name:     "archive without main binary",
			options:  InstallOptions{Version: "b040", Keep: DefaultKeep},
			wantErr:  "contains no llama-server",
			wantGone: []string{"b040-vulkan"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newManagerFixture(t, "")
			for _, options := range tt.before {
				fixture.install(t, options)
			}
			fixture.output.Reset()

			err := fixture.manager.Install(context.Background(), tt.options)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Install() error = %v, want it to contain %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Install() error = %v\n%s", err, fixture.output)
			}
			if tt.wantCurrent != "" && fixture.current(t) != tt.wantCurrent {
				t.Fatalf("current = %q, want %q", fixture.current(t), tt.wantCurrent)
			}
			for _, folder := range tt.wantFolders {
				if !fixture.exists(folder) {
					t.Fatalf("%s missing", folder)
				}
			}
			for _, folder := range tt.wantGone {
				if fixture.exists(folder) {
					t.Fatalf("%s should not exist", folder)
				}
			}
			for _, text := range tt.wantOutput {
				if !strings.Contains(fixture.output.String(), text) {
					t.Fatalf("output %q does not contain %q", fixture.output, text)
				}
			}
			for _, text := range tt.wantNoOutput {
				if strings.Contains(fixture.output.String(), text) {
					t.Fatalf("output %q contains %q", fixture.output, text)
				}
			}
			if tt.wantDownloads > 0 && len(fixture.fake.downloads) != tt.wantDownloads {
				t.Fatalf("downloads = %v, want %d", fixture.fake.downloads, tt.wantDownloads)
			}
			entries, _ := os.ReadDir(fixture.root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), stagingPrefix) {
					t.Fatalf("staging folder %s left behind", entry.Name())
				}
			}
		})
	}
}

func TestManagerInstallWritesStampAndLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bin links are not used on windows")
	}
	fixture := newManagerFixture(t, "")

	fixture.install(t, InstallOptions{Keep: DefaultKeep})

	if stamp := ReadStamp(filepath.Join(fixture.root, "b500-vulkan")); stamp != (Stamp{Build: "b500", Variant: "vulkan", Stable: "v2.0.0"}) {
		t.Fatalf("stamp = %+v", stamp)
	}
	content, err := os.ReadFile(filepath.Join(fixture.binDir, "llama-server"))
	if err != nil || string(content) != "b500" {
		t.Fatalf("bin/llama-server = %q (%v), want b500", content, err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.binDir, "libggml-base.so")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shared library was linked: %v", err)
	}
}

func TestManagerInstallWindowsMarker(t *testing.T) {
	fake := newFakeGitHub(t, "b1", []fakeRelease{{tag: "b1", assets: map[string][]byte{
		"llama-b1-bin-win-cpu-x64.zip": buildZip(t, []testEntry{{name: "llama-server.exe", body: "exe", mode: 0o644}}),
	}}})
	root := filepath.Join(t.TempDir(), "llama.cpp")
	manager := New(withRepository(backend.LlamaCpp), fake.client(), backend.Platform{OS: "windows", Arch: "amd64"}, root, filepath.Join(t.TempDir(), "bin"))
	output := &bytes.Buffer{}
	manager.Output = output

	if err := manager.Install(context.Background(), InstallOptions{Variant: "cpu", Keep: DefaultKeep}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}

	marker, err := os.ReadFile(filepath.Join(root, CurrentName))
	if err != nil || strings.TrimSpace(string(marker)) != "b1-cpu" {
		t.Fatalf("current marker = %q (%v)", marker, err)
	}
	if !strings.Contains(output.String(), filepath.Join(root, "b1-cpu", "llama-server.exe")) {
		t.Fatalf("output %q does not print the binary path", output)
	}
}

func TestManagerUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses symlinks")
	}

	tests := []struct {
		name        string
		argument    string
		input       string
		empty       bool
		wantErr     string
		wantCurrent string
		wantOutput  string
	}{
		{name: "by version", argument: "b200", wantCurrent: "b200-vulkan"},
		{name: "by index", argument: "3", wantCurrent: "b100-cpu"},
		{name: "by folder", argument: "b200-vulkan", wantCurrent: "b200-vulkan"},
		{name: "already current", argument: "b300", wantCurrent: "b300-vulkan", wantOutput: "already current"},
		{name: "menu pick", input: "2\n", wantCurrent: "b200-vulkan", wantOutput: "pick a number"},
		{name: "menu cancel", input: "\n", wantCurrent: "b300-vulkan", wantOutput: "cancelled"},
		{name: "not installed hints install", argument: "b999", wantErr: "aim backend llama.cpp add b999"},
		{name: "no builds", argument: "b1", empty: true, wantErr: "no builds in"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newManagerFixture(t, tt.input)
			if !tt.empty {
				fixture.install(t, InstallOptions{Version: "b100", Variant: "cpu", Keep: DefaultKeep})
				fixture.install(t, InstallOptions{Version: "b200", Variant: "vulkan", Keep: DefaultKeep})
				fixture.install(t, InstallOptions{Version: "b300", Keep: DefaultKeep})
			}
			fixture.output.Reset()

			err := fixture.manager.Use(tt.argument)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Use() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Use() error = %v", err)
			}
			if got := fixture.current(t); got != tt.wantCurrent {
				t.Fatalf("current = %q, want %q", got, tt.wantCurrent)
			}
			if !strings.Contains(fixture.output.String(), tt.wantOutput) {
				t.Fatalf("output %q does not contain %q", fixture.output, tt.wantOutput)
			}
		})
	}
}

func TestManagerUseAmbiguousListsBuilds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses symlinks")
	}
	fixture := newManagerFixture(t, "")
	fixture.install(t, InstallOptions{Version: "b400", Variant: "vulkan", Keep: DefaultKeep})
	fixture.install(t, InstallOptions{Version: "b400", Variant: "rocm", Keep: DefaultKeep})
	fixture.install(t, InstallOptions{Version: "b300", Variant: "vulkan", Keep: DefaultKeep})

	err := fixture.manager.Use("b400")

	var ambiguous *AmbiguousBuildError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Use() error = %v, want AmbiguousBuildError", err)
	}
	for _, text := range []string{"b400-vulkan", "b400-rocm", "1  ", "2  "} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error %q does not contain %q", err, text)
		}
	}
}

func TestManagerRemove(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses symlinks")
	}

	tests := []struct {
		name        string
		arguments   []string
		confirmed   bool
		interactive bool
		input       string
		wantErr     string
		wantGone    []string
		wantKept    []string
	}{
		{name: "confirmed by flag", arguments: []string{"b200"}, confirmed: true, wantGone: []string{"b200-vulkan"}},
		{name: "several targets deduplicated", arguments: []string{"b200", "2", "b100-cpu"}, confirmed: true, wantGone: []string{"b200-vulkan", "b100-cpu"}},
		{name: "refuses active build", arguments: []string{"b300"}, confirmed: true, wantErr: "refusing to delete the active build", wantKept: []string{"b300-vulkan"}},
		{name: "non tty without yes", arguments: []string{"b200"}, wantErr: "not a terminal", wantKept: []string{"b200-vulkan"}},
		{name: "interactive yes", arguments: []string{"b200"}, interactive: true, input: "y\n", wantGone: []string{"b200-vulkan"}},
		{name: "interactive no", arguments: []string{"b200"}, interactive: true, input: "n\n", wantKept: []string{"b200-vulkan"}},
		{name: "no arguments", wantErr: "rm needs"},
		{name: "unknown build", arguments: []string{"b999"}, confirmed: true, wantErr: "no installed build matches"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newManagerFixture(t, tt.input)
			fixture.install(t, InstallOptions{Version: "b100", Variant: "cpu", Keep: DefaultKeep})
			fixture.install(t, InstallOptions{Version: "b200", Variant: "vulkan", Keep: DefaultKeep})
			fixture.install(t, InstallOptions{Version: "b300", Keep: DefaultKeep})
			fixture.manager.Interactive = tt.interactive

			err := fixture.manager.Remove(tt.arguments, tt.confirmed)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Remove() error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Remove() error = %v", err)
			}
			for _, folder := range tt.wantGone {
				if fixture.exists(folder) {
					t.Fatalf("%s still exists", folder)
				}
			}
			for _, folder := range tt.wantKept {
				if !fixture.exists(folder) {
					t.Fatalf("%s was deleted", folder)
				}
			}
		})
	}
}

func TestManagerList(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses symlinks")
	}

	tests := []struct {
		name         string
		options      ListOptions
		wantOutput   []string
		wantNoOutput []string
	}{
		{
			name:         "local only",
			options:      ListOptions{Local: true},
			wantOutput:   []string{"Installed", "*  1  b500-vulkan", "active v2.0.0", "2  b100-cpu", "manual-build", "manual"},
			wantNoOutput: []string{"Available"},
		},
		{
			name:       "with upstream",
			options:    ListOptions{Limit: 2},
			wantOutput: []string{"Available", "stable", "v2.0.0    -> b500", "installed: vulkan", "b400", "build"},
			wantNoOutput: []string{
				"b300 ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newManagerFixture(t, "")
			fixture.install(t, InstallOptions{Version: "b100", Variant: "cpu", Keep: DefaultKeep})
			fixture.install(t, InstallOptions{Variant: "vulkan", Keep: DefaultKeep})
			manual := filepath.Join(fixture.root, "manual-build")
			writeTestFile(t, filepath.Join(manual, "llama-server"), 0o755)
			fixture.output.Reset()
			fixture.fake.downloads = nil

			if err := fixture.manager.List(context.Background(), tt.options); err != nil {
				t.Fatalf("List() error = %v", err)
			}

			for _, text := range tt.wantOutput {
				if !strings.Contains(fixture.output.String(), text) {
					t.Fatalf("output does not contain %q:\n%s", text, fixture.output)
				}
			}
			for _, text := range tt.wantNoOutput {
				if strings.Contains(fixture.output.String(), text) {
					t.Fatalf("output contains %q:\n%s", text, fixture.output)
				}
			}
			if tt.options.Local && len(fixture.fake.downloads) != 0 {
				t.Fatalf("local list hit the network: %v", fixture.fake.downloads)
			}
		})
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		{name: "bytes", bytes: 512, want: "512B"},
		{name: "kilobytes", bytes: 2048, want: "2K"},
		{name: "megabytes", bytes: 3 * 1024 * 1024 / 2, want: "1.5M"},
		{name: "gigabytes", bytes: 5 * 1024 * 1024 * 1024, want: "5.0G"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HumanSize(tt.bytes); got != tt.want {
				t.Fatalf("HumanSize() = %q, want %q", got, tt.want)
			}
		})
	}
}
