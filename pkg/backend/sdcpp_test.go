package backend

import "testing"

var sdReleaseAssets = []string{
	"cudart-sd-bin-win-cu12-x64.zip",
	"sd-master-37bc88b-bin-Darwin-macOS-26.6.2-arm64.zip",
	"sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64-rocm-6.4.1.zip",
	"sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64-rocm-10.1.0.zip",
	"sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip",
	"sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64.zip",
	"sd-master-37bc88b-bin-win-cpu-x64.zip",
	"sd-master-37bc88b-bin-win-cuda12-x64.zip",
	"sd-master-37bc88b-bin-win-rocm-10.1.0-x64.zip",
	"sd-master-37bc88b-bin-win-vulkan-x64.zip",
}

func TestSDCppPickAsset(t *testing.T) {
	release := releaseOf(sdReleaseAssets)

	tests := []struct {
		name     string
		platform Platform
		variant  string
		wantName string
	}{
		{name: "linux vulkan", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "vulkan", wantName: "sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64-vulkan.zip"},
		{name: "linux rocm newest", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "rocm", wantName: "sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64-rocm-10.1.0.zip"},
		{name: "linux cpu", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "cpu", wantName: "sd-master-37bc88b-bin-Linux-Ubuntu-24.04-x86_64.zip"},
		{name: "darwin metal", platform: Platform{OS: "darwin", Arch: "arm64"}, variant: "metal", wantName: "sd-master-37bc88b-bin-Darwin-macOS-26.6.2-arm64.zip"},
		{name: "windows vulkan", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "vulkan", wantName: "sd-master-37bc88b-bin-win-vulkan-x64.zip"},
		{name: "windows cuda", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "cuda", wantName: "sd-master-37bc88b-bin-win-cuda12-x64.zip"},
		{name: "windows rocm", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "rocm", wantName: "sd-master-37bc88b-bin-win-rocm-10.1.0-x64.zip"},
		{name: "linux arm64 missing", platform: Platform{OS: "linux", Arch: "arm64"}, variant: "vulkan"},
		{name: "sycl unsupported", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "sycl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SDCpp.PickAsset(release, tt.platform, tt.variant)
			if ok != (tt.wantName != "") {
				t.Fatalf("PickAsset() ok = %v, want asset %q", ok, tt.wantName)
			}
			if got.Name != tt.wantName {
				t.Fatalf("PickAsset() = %q, want %q", got.Name, tt.wantName)
			}
			if ok && got.Format != ArchiveZip {
				t.Fatalf("PickAsset() Format = %q, want %q", got.Format, ArchiveZip)
			}
		})
	}
}
