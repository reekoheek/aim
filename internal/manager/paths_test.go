package manager

import (
	"path/filepath"
	"testing"
)

func TestRootEnvName(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		want    string
	}{
		{name: "llama.cpp", backend: "llama.cpp", want: "AIM_LLAMA_CPP_ROOT"},
		{name: "sd.cpp", backend: "sd.cpp", want: "AIM_SD_CPP_ROOT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RootEnvName(tt.backend); got != tt.want {
				t.Fatalf("RootEnvName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvironmentPaths(t *testing.T) {
	absolute := func(path string) string {
		resolved, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}

	tests := []struct {
		name         string
		environment  Environment
		rootOverride string
		binOverride  string
		wantRoot     string
		wantBinDir   string
		wantRootErr  bool
	}{
		{
			name:        "linux default",
			environment: Environment{OS: "linux", Home: "/home/u"},
			wantRoot:    filepath.Join("/home/u", ".local", "share", "llama.cpp"),
			wantBinDir:  filepath.Join("/home/u", ".local", "bin"),
		},
		{
			name:        "linux xdg data home",
			environment: Environment{OS: "linux", Home: "/home/u", Lookup: lookupOf(map[string]string{"XDG_DATA_HOME": absolute("/data")})},
			wantRoot:    filepath.Join(absolute("/data"), "llama.cpp"),
			wantBinDir:  filepath.Join("/home/u", ".local", "bin"),
		},
		{
			name:        "darwin default",
			environment: Environment{OS: "darwin", Home: "/Users/u"},
			wantRoot:    filepath.Join("/Users/u", "Library", "Application Support", "llama.cpp"),
			wantBinDir:  filepath.Join("/Users/u", ".local", "bin"),
		},
		{
			name:        "windows local app data",
			environment: Environment{OS: "windows", Home: "C:/Users/u", Lookup: lookupOf(map[string]string{"LocalAppData": "C:/Users/u/AppData/Local"})},
			wantRoot:    filepath.Join("C:/Users/u/AppData/Local", "llama.cpp"),
			wantBinDir:  filepath.Join("C:/Users/u", ".local", "bin"),
		},
		{
			name:        "windows without local app data",
			environment: Environment{OS: "windows", Home: "C:/Users/u"},
			wantBinDir:  filepath.Join("C:/Users/u", ".local", "bin"),
			wantRootErr: true,
		},
		{
			name:         "flag overrides",
			environment:  Environment{OS: "linux", Home: "/home/u", Lookup: lookupOf(map[string]string{"AIM_LLAMA_CPP_ROOT": "/env", BinDirEnv: "/envbin"})},
			rootOverride: "/home/u/.local/share/llama.cpp",
			binOverride:  "/flagbin",
			wantRoot:     absolute("/home/u/.local/share/llama.cpp"),
			wantBinDir:   absolute("/flagbin"),
		},
		{
			name:        "env overrides",
			environment: Environment{OS: "linux", Home: "/home/u", Lookup: lookupOf(map[string]string{"AIM_LLAMA_CPP_ROOT": "/env", BinDirEnv: "/envbin"})},
			wantRoot:    absolute("/env"),
			wantBinDir:  absolute("/envbin"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.environment.Lookup == nil {
				tt.environment.Lookup = lookupOf(nil)
			}
			root, err := tt.environment.Root("llama.cpp", tt.rootOverride)
			if (err != nil) != tt.wantRootErr {
				t.Fatalf("Root() error = %v, wantErr %v", err, tt.wantRootErr)
			}
			if root != tt.wantRoot {
				t.Fatalf("Root() = %q, want %q", root, tt.wantRoot)
			}
			binDir, err := tt.environment.BinDir(tt.binOverride)
			if err != nil || binDir != tt.wantBinDir {
				t.Fatalf("BinDir() = (%q, %v), want %q", binDir, err, tt.wantBinDir)
			}
		})
	}
}

func lookupOf(values map[string]string) func(key string) string {
	return func(key string) string {
		return values[key]
	}
}
