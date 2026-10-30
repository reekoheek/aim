package backend

import "testing"

var llamaReleaseAssets = []string{
	"cudart-llama-b12002-bin-ubuntu-cuda-12.8-x64.tar.gz",
	"cudart-llama-b12002-bin-ubuntu-cuda-13.4-x64.tar.gz",
	"cudart-llama-bin-win-cuda-13.4-x64.zip",
	"llama-b12002-bin-android-arm64.tar.gz",
	"llama-b12002-bin-linux-arm64-snapdragon.tar.gz",
	"llama-b12002-bin-macos-arm64.tar.gz",
	"llama-b12002-bin-macos-x64.tar.gz",
	"llama-b12002-bin-ubuntu-arm64.tar.gz",
	"llama-b12002-bin-ubuntu-cuda-12.8-x64.tar.gz",
	"llama-b12002-bin-ubuntu-cuda-13.4-arm64.tar.gz",
	"llama-b12002-bin-ubuntu-cuda-13.4-x64.tar.gz",
	"llama-b12002-bin-ubuntu-openvino-2026.4.1-x64.tar.gz",
	"llama-b12002-bin-ubuntu-rocm-7.2-x64.tar.gz",
	"llama-b12002-bin-ubuntu-rocm-10.0-x64.tar.gz",
	"llama-b12002-bin-ubuntu-sycl-fp16-x64.tar.gz",
	"llama-b12002-bin-ubuntu-sycl-fp32-x64.tar.gz",
	"llama-b12002-bin-ubuntu-vulkan-arm64.tar.gz",
	"llama-b12002-bin-ubuntu-vulkan-x64.tar.gz",
	"llama-b12002-bin-ubuntu-x64.tar.gz",
	"llama-b12002-bin-win-cpu-arm64.zip",
	"llama-b12002-bin-win-cpu-x64.zip",
	"llama-b12002-bin-win-cuda-12.4-x64.zip",
	"llama-b12002-bin-win-cuda-13.4-x64.zip",
	"llama-b12002-bin-win-rocm-10.0-x64.zip",
	"llama-b12002-bin-win-sycl-x64.zip",
	"llama-b12002-bin-win-vulkan-x64.zip",
	"llama-b12002-ui.tar.gz",
	"llama-b12002-xcframework.zip",
}

func releaseOf(names []string) Release {
	release := Release{}
	for _, name := range names {
		release.Assets = append(release.Assets, Asset{Name: name, URL: "https://example.test/" + name})
	}
	return release
}

func TestLlamaCppPickAsset(t *testing.T) {
	release := releaseOf(llamaReleaseAssets)

	tests := []struct {
		name       string
		platform   Platform
		variant    string
		wantName   string
		wantFormat ArchiveFormat
	}{
		{name: "linux vulkan", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "vulkan", wantName: "llama-b12002-bin-ubuntu-vulkan-x64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "linux rocm newest", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "rocm", wantName: "llama-b12002-bin-ubuntu-rocm-10.0-x64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "linux cpu", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "cpu", wantName: "llama-b12002-bin-ubuntu-x64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "linux sycl fp16", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "sycl", wantName: "llama-b12002-bin-ubuntu-sycl-fp16-x64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "linux cuda newest skips cudart", platform: Platform{OS: "linux", Arch: "amd64"}, variant: "cuda", wantName: "llama-b12002-bin-ubuntu-cuda-13.4-x64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "linux arm64 vulkan", platform: Platform{OS: "linux", Arch: "arm64"}, variant: "vulkan", wantName: "llama-b12002-bin-ubuntu-vulkan-arm64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "linux arm64 cpu", platform: Platform{OS: "linux", Arch: "arm64"}, variant: "cpu", wantName: "llama-b12002-bin-ubuntu-arm64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "darwin metal", platform: Platform{OS: "darwin", Arch: "arm64"}, variant: "metal", wantName: "llama-b12002-bin-macos-arm64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "darwin intel cpu", platform: Platform{OS: "darwin", Arch: "amd64"}, variant: "cpu", wantName: "llama-b12002-bin-macos-x64.tar.gz", wantFormat: ArchiveTarGz},
		{name: "windows vulkan", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "vulkan", wantName: "llama-b12002-bin-win-vulkan-x64.zip", wantFormat: ArchiveZip},
		{name: "windows cuda newest", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "cuda", wantName: "llama-b12002-bin-win-cuda-13.4-x64.zip", wantFormat: ArchiveZip},
		{name: "windows rocm", platform: Platform{OS: "windows", Arch: "amd64"}, variant: "rocm", wantName: "llama-b12002-bin-win-rocm-10.0-x64.zip", wantFormat: ArchiveZip},
		{name: "windows arm64 cpu", platform: Platform{OS: "windows", Arch: "arm64"}, variant: "cpu", wantName: "llama-b12002-bin-win-cpu-arm64.zip", wantFormat: ArchiveZip},
		{name: "windows arm64 vulkan missing", platform: Platform{OS: "windows", Arch: "arm64"}, variant: "vulkan"},
		{name: "darwin vulkan missing", platform: Platform{OS: "darwin", Arch: "arm64"}, variant: "vulkan"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := LlamaCpp.PickAsset(release, tt.platform, tt.variant)
			if ok != (tt.wantName != "") {
				t.Fatalf("PickAsset() ok = %v, want asset %q", ok, tt.wantName)
			}
			if got.Name != tt.wantName || got.Format != tt.wantFormat {
				t.Fatalf("PickAsset() = (%q, %q), want (%q, %q)", got.Name, got.Format, tt.wantName, tt.wantFormat)
			}
		})
	}
}
