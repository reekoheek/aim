package backend

import "regexp"

const sdAssetPrefix = `^sd-master-[0-9a-f]+-bin-`

func sdAsset(operatingSystem string, arch string, variant string, middle string) AssetPattern {
	return AssetPattern{
		OS:      operatingSystem,
		Arch:    arch,
		Variant: variant,
		Format:  ArchiveZip,
		Name:    regexp.MustCompile(sdAssetPrefix + middle + `\.zip$`),
	}
}

var SDCpp = Descriptor{
	Name:               "sd.cpp",
	Repository:         "leejet/stable-diffusion.cpp",
	BuildTag:           regexp.MustCompile(`^master-(\d+)-[0-9a-f]+$`),
	Variants:           []string{"vulkan", "rocm", "cpu", "cuda", "metal"},
	DefaultVariant:     "vulkan",
	DefaultVariantByOS: map[string]string{"darwin": "metal"},
	Assets: []AssetPattern{
		sdAsset("linux", "amd64", "vulkan", `Linux-Ubuntu-[0-9.]+-x86_64-vulkan`),
		sdAsset("linux", "amd64", "rocm", `Linux-Ubuntu-[0-9.]+-x86_64-rocm-([0-9.]+)`),
		sdAsset("linux", "amd64", "cpu", `Linux-Ubuntu-[0-9.]+-x86_64`),
		sdAsset("darwin", "arm64", "metal", `Darwin-macOS-[0-9.]+-arm64`),
		sdAsset("windows", "amd64", "vulkan", `win-vulkan-x64`),
		sdAsset("windows", "amd64", "rocm", `win-rocm-([0-9.]+)-x64`),
		sdAsset("windows", "amd64", "cpu", `win-cpu-x64`),
		sdAsset("windows", "amd64", "cuda", `win-cuda([0-9.]+)-x64`),
	},
	MainBinary:     "sd-server",
	LinkedBinaries: []string{"sd-server", "sd-cli"},
}
