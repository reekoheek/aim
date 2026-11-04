package backend

import (
	"reflect"
	"regexp"
	"testing"
)

func TestDescriptorParseVersion(t *testing.T) {
	tests := []struct {
		name       string
		descriptor Descriptor
		spec       string
		want       Version
		wantErr    bool
	}{
		{name: "llama empty is latest stable", descriptor: LlamaCpp, spec: "", want: Version{Kind: VersionLatest}},
		{name: "llama latest", descriptor: LlamaCpp, spec: "latest", want: Version{Kind: VersionLatest}},
		{name: "llama stable", descriptor: LlamaCpp, spec: "stable", want: Version{Kind: VersionLatest}},
		{name: "llama dev", descriptor: LlamaCpp, spec: "dev", want: Version{Kind: VersionDev}},
		{name: "llama nightly", descriptor: LlamaCpp, spec: "nightly", want: Version{Kind: VersionDev}},
		{name: "llama full stable tag", descriptor: LlamaCpp, spec: "v1.2.3", want: Version{Kind: VersionStable, Tag: "v1.2.3"}},
		{name: "llama stable without v", descriptor: LlamaCpp, spec: "0.2.0", want: Version{Kind: VersionStable, Tag: "v0.2.0"}},
		{name: "llama short stable", descriptor: LlamaCpp, spec: "v0.2", want: Version{Kind: VersionStable, Tag: "v0.2.0"}},
		{name: "llama build tag", descriptor: LlamaCpp, spec: "b6500", want: Version{Kind: VersionBuild, Tag: "b6500", Number: 6500}},
		{name: "llama bare build number", descriptor: LlamaCpp, spec: "10590", want: Version{Kind: VersionBuild, Tag: "b10590", Number: 10590}},
		{name: "llama rejects garbage", descriptor: LlamaCpp, spec: "foo", wantErr: true},
		{name: "llama rejects short number", descriptor: LlamaCpp, spec: "12", wantErr: true},
		{name: "sd latest is dev", descriptor: SDCpp, spec: "latest", want: Version{Kind: VersionDev}},
		{name: "sd empty is dev", descriptor: SDCpp, spec: "", want: Version{Kind: VersionDev}},
		{name: "sd dev", descriptor: SDCpp, spec: "dev", want: Version{Kind: VersionDev}},
		{name: "sd full tag", descriptor: SDCpp, spec: "master-412-abc123", want: Version{Kind: VersionBuild, Tag: "master-412-abc123", Number: 412}},
		{name: "sd bare number has no tag", descriptor: SDCpp, spec: "412", want: Version{Kind: VersionBuild, Number: 412}},
		{name: "sd rejects stable", descriptor: SDCpp, spec: "v1.2.3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.descriptor.ParseVersion(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseVersion(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseVersion(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestDescriptorTags(t *testing.T) {
	tests := []struct {
		name       string
		descriptor Descriptor
		tag        string
		wantBuild  bool
		wantStable bool
		wantNumber int
	}{
		{name: "llama build", descriptor: LlamaCpp, tag: "b6500", wantBuild: true, wantNumber: 6500},
		{name: "llama stable", descriptor: LlamaCpp, tag: "v0.8.0", wantStable: true},
		{name: "llama other", descriptor: LlamaCpp, tag: "master-1-abc"},
		{name: "sd build", descriptor: SDCpp, tag: "master-1028-37bc88b", wantBuild: true, wantNumber: 1028},
		{name: "sd never stable", descriptor: SDCpp, tag: "v0.8.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.descriptor.IsBuildTag(tt.tag); got != tt.wantBuild {
				t.Fatalf("IsBuildTag(%q) = %v, want %v", tt.tag, got, tt.wantBuild)
			}
			if got := tt.descriptor.IsStableTag(tt.tag); got != tt.wantStable {
				t.Fatalf("IsStableTag(%q) = %v, want %v", tt.tag, got, tt.wantStable)
			}
			number, _ := tt.descriptor.BuildNumber(tt.tag)
			if number != tt.wantNumber {
				t.Fatalf("BuildNumber(%q) = %d, want %d", tt.tag, number, tt.wantNumber)
			}
		})
	}
}

func TestDescriptorPickAsset(t *testing.T) {
	descriptor := Descriptor{
		Name:     "fake",
		BuildTag: regexp.MustCompile(`^b(\d+)$`),
		Variants: []string{"cpu", "rocm"},
		Assets: []AssetPattern{
			{OS: "linux", Arch: "amd64", Variant: "cpu", Format: ArchiveTarGz, Name: regexp.MustCompile(`^fake-linux-x64\.tar\.gz$`)},
			{OS: "linux", Arch: "amd64", Variant: "rocm", Format: ArchiveTarGz, Name: regexp.MustCompile(`^fake-linux-rocm-([0-9.]+)-x64\.tar\.gz$`)},
		},
	}
	release := Release{Assets: []Asset{
		{Name: "fake-linux-x64.tar.gz", URL: "cpu"},
		{Name: "fake-linux-rocm-6.4-x64.tar.gz", URL: "rocm-6.4"},
		{Name: "fake-linux-rocm-10.0-x64.tar.gz", URL: "rocm-10.0"},
		{Name: "fake-linux-rocm-7.2.1-x64.tar.gz", URL: "rocm-7.2.1"},
	}}

	tests := []struct {
		name     string
		platform Platform
		variant  string
		wantURL  string
		wantOK   bool
	}{
		{name: "plain variant", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "cpu", wantURL: "cpu", wantOK: true},
		{name: "newest toolkit wins", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "rocm", wantURL: "rocm-10.0", wantOK: true},
		{name: "unknown variant", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "vulkan"},
		{name: "unknown platform", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "cpu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := descriptor.PickAsset(release, tt.platform, tt.variant)
			if ok != tt.wantOK {
				t.Fatalf("PickAsset() ok = %v, want %v", ok, tt.wantOK)
			}
			if got.URL != tt.wantURL {
				t.Fatalf("PickAsset() URL = %q, want %q", got.URL, tt.wantURL)
			}
			if ok && got.Format != ArchiveTarGz {
				t.Fatalf("PickAsset() Format = %q, want %q", got.Format, ArchiveTarGz)
			}
		})
	}
}

func TestDescriptorParseFolderName(t *testing.T) {
	tests := []struct {
		name        string
		descriptor  Descriptor
		folder      string
		wantBuild   string
		wantVariant string
		wantOK      bool
	}{
		{name: "llama folder", descriptor: LlamaCpp, folder: "b6500-vulkan", wantBuild: "b6500", wantVariant: "vulkan", wantOK: true},
		{name: "llama-manager folder", descriptor: LlamaCpp, folder: "llama-b11951-rocm", wantBuild: "b11951", wantVariant: "rocm", wantOK: true},
		{name: "sd folder", descriptor: SDCpp, folder: "master-412-abc123-vulkan", wantBuild: "master-412-abc123", wantVariant: "vulkan", wantOK: true},
		{name: "unknown variant", descriptor: LlamaCpp, folder: "b6500-opencl"},
		{name: "hand placed", descriptor: LlamaCpp, folder: "my-build"},
		{name: "no separator", descriptor: LlamaCpp, folder: "b6500"},
		{name: "sd legacy prefix not accepted", descriptor: SDCpp, folder: "llama-master-1-abc-cpu"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			build, variant, ok := tt.descriptor.ParseFolderName(tt.folder)
			if ok != tt.wantOK || build != tt.wantBuild || variant != tt.wantVariant {
				t.Fatalf("ParseFolderName(%q) = (%q, %q, %v), want (%q, %q, %v)", tt.folder, build, variant, ok, tt.wantBuild, tt.wantVariant, tt.wantOK)
			}
			if ok && tt.descriptor.LegacyFolderPrefix == "" {
				if folder := tt.descriptor.FolderName(build, variant); folder != tt.folder {
					t.Fatalf("FolderName() = %q, want %q", folder, tt.folder)
				}
			}
		})
	}
}

func TestDescriptorDefaults(t *testing.T) {
	tests := []struct {
		name            string
		descriptor      Descriptor
		platform        Platform
		build           string
		wantVariant     string
		wantStripPrefix string
	}{
		{name: "llama linux", descriptor: LlamaCpp, platform: Platform{OS: "linux", Arch: "amd64"}, build: "b1", wantVariant: "vulkan", wantStripPrefix: "llama-b1/"},
		{name: "llama darwin", descriptor: LlamaCpp, platform: Platform{OS: "darwin", Arch: "arm64"}, build: "b1", wantVariant: "metal", wantStripPrefix: "llama-b1/"},
		{name: "llama darwin intel falls back to cpu", descriptor: LlamaCpp, platform: Platform{OS: "darwin", Arch: "amd64"}, build: "b1", wantVariant: "cpu", wantStripPrefix: "llama-b1/"},
		{name: "unsupported platform keeps default", descriptor: LlamaCpp, platform: Platform{OS: "plan9", Arch: "amd64"}, build: "b1", wantVariant: "vulkan", wantStripPrefix: "llama-b1/"},
		{name: "sd windows", descriptor: SDCpp, platform: Platform{OS: "windows", Arch: "amd64"}, build: "master-1-abc", wantVariant: "vulkan"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.descriptor.DefaultVariantFor(tt.platform); got != tt.wantVariant {
				t.Fatalf("DefaultVariantFor() = %q, want %q", got, tt.wantVariant)
			}
			if got := tt.descriptor.StripPrefixFor(tt.build); got != tt.wantStripPrefix {
				t.Fatalf("StripPrefixFor() = %q, want %q", got, tt.wantStripPrefix)
			}
		})
	}
}

func TestExecutableName(t *testing.T) {
	tests := []struct {
		name            string
		operatingSystem string
		want            string
	}{
		{name: "linux", operatingSystem: "linux", want: "llama-server"},
		{name: "darwin", operatingSystem: "darwin", want: "llama-server"},
		{name: "windows", operatingSystem: "windows", want: "llama-server.exe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExecutableName("llama-server", tt.operatingSystem); got != tt.want {
				t.Fatalf("ExecutableName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	tests := []struct {
		name   string
		lookup string
		wantOK bool
	}{
		{name: "llama.cpp", lookup: "llama.cpp", wantOK: true},
		{name: "sd.cpp", lookup: "sd.cpp", wantOK: true},
		{name: "no alias", lookup: "llama", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			descriptor, ok := Lookup(tt.lookup)
			if ok != tt.wantOK {
				t.Fatalf("Lookup(%q) ok = %v, want %v", tt.lookup, ok, tt.wantOK)
			}
			if ok && descriptor.Name != tt.lookup {
				t.Fatalf("Lookup(%q).Name = %q", tt.lookup, descriptor.Name)
			}
		})
	}

	if got := Names(); !reflect.DeepEqual(got, []string{"llama.cpp", "sd.cpp"}) {
		t.Fatalf("Names() = %v", got)
	}
}

func TestReleaseAsset(t *testing.T) {
	release := Release{Assets: []Asset{{Name: "nightly-tag.txt", URL: "u"}}}

	tests := []struct {
		name    string
		asset   string
		wantURL string
		wantOK  bool
	}{
		{name: "present", asset: "nightly-tag.txt", wantURL: "u", wantOK: true},
		{name: "missing", asset: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := release.Asset(tt.asset)
			if ok != tt.wantOK || got.URL != tt.wantURL {
				t.Fatalf("Asset(%q) = (%+v, %v)", tt.asset, got, ok)
			}
		})
	}
}
