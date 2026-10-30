package manager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStamp(t *testing.T) {
	tests := []struct {
		name    string
		write   *Stamp
		files   map[string]string
		want    Stamp
		wantRaw string
	}{
		{
			name:    "round trip with stable",
			write:   &Stamp{Build: "b6500", Variant: "vulkan", Stable: "v1.2.3"},
			want:    Stamp{Build: "b6500", Variant: "vulkan", Stable: "v1.2.3"},
			wantRaw: "build=b6500\nvariant=vulkan\nstable=v1.2.3\n",
		},
		{
			name:    "round trip without stable",
			write:   &Stamp{Build: "master-1-abc", Variant: "cpu"},
			want:    Stamp{Build: "master-1-abc", Variant: "cpu"},
			wantRaw: "build=master-1-abc\nvariant=cpu\n",
		},
		{
			name:  "legacy llama-manager stamp",
			files: map[string]string{legacyStampFileName: "build=b11951\nvariant=vulkan\nstable=v0.8.0\n"},
			want:  Stamp{Build: "b11951", Variant: "vulkan", Stable: "v0.8.0"},
		},
		{
			name:  "own stamp wins over legacy",
			files: map[string]string{legacyStampFileName: "build=b1\n", StampFileName: "build=b2\n"},
			want:  Stamp{Build: "b2"},
		},
		{
			name:  "ignores junk lines",
			files: map[string]string{StampFileName: "junk\nbuild=b3\nother=x\n"},
			want:  Stamp{Build: "b3"},
		},
		{
			name: "missing stamp",
			want: Stamp{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.write != nil {
				if err := WriteStamp(directory, *tt.write); err != nil {
					t.Fatalf("WriteStamp() error = %v", err)
				}
				raw, _ := os.ReadFile(filepath.Join(directory, StampFileName))
				if string(raw) != tt.wantRaw {
					t.Fatalf("stamp content = %q, want %q", raw, tt.wantRaw)
				}
			}
			if got := ReadStamp(directory); got != tt.want {
				t.Fatalf("ReadStamp() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
