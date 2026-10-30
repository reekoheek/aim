package backend

import "regexp"

const (
	llamaAssetPrefix = `^llama-b\d+-bin-`
	llamaTarGz       = `\.tar\.gz$`
	llamaZip         = `\.zip$`
)

func llamaAsset(operatingSystem string, arch string, variant string, format ArchiveFormat, middle string) AssetPattern {
	suffix := llamaTarGz
	if format == ArchiveZip {
		suffix = llamaZip
	}
	return AssetPattern{
		OS:      operatingSystem,
		Arch:    arch,
		Variant: variant,
		Format:  format,
		Name:    regexp.MustCompile(llamaAssetPrefix + middle + suffix),
	}
}

var LlamaCpp = Descriptor{
	Name:               "llama.cpp",
	Repository:         "ggml-org/llama.cpp",
	BuildTag:           regexp.MustCompile(`^b(\d+)$`),
	BuildTagFormat:     "b%d",
	StableTag:          regexp.MustCompile(`^v\d+\.\d+\.\d+$`),
	StableBuildAsset:   "nightly-tag.txt",
	LegacyFolderPrefix: "llama-",
	Variants:           []string{"vulkan", "rocm", "cpu", "sycl", "cuda", "metal"},
	DefaultVariant:     "vulkan",
	DefaultVariantByOS: map[string]string{"darwin": "metal"},
	Assets: []AssetPattern{
		llamaAsset("linux", "amd64", "vulkan", ArchiveTarGz, `ubuntu-vulkan-x64`),
		llamaAsset("linux", "amd64", "rocm", ArchiveTarGz, `ubuntu-rocm-([0-9.]+)-x64`),
		llamaAsset("linux", "amd64", "cpu", ArchiveTarGz, `ubuntu-x64`),
		llamaAsset("linux", "amd64", "sycl", ArchiveTarGz, `ubuntu-sycl-fp16-x64`),
		llamaAsset("linux", "amd64", "cuda", ArchiveTarGz, `ubuntu-cuda-([0-9.]+)-x64`),
		llamaAsset("linux", "arm64", "vulkan", ArchiveTarGz, `ubuntu-vulkan-arm64`),
		llamaAsset("linux", "arm64", "cpu", ArchiveTarGz, `ubuntu-arm64`),
		llamaAsset("linux", "arm64", "cuda", ArchiveTarGz, `ubuntu-cuda-([0-9.]+)-arm64`),
		llamaAsset("darwin", "arm64", "metal", ArchiveTarGz, `macos-arm64`),
		llamaAsset("darwin", "amd64", "cpu", ArchiveTarGz, `macos-x64`),
		llamaAsset("windows", "amd64", "vulkan", ArchiveZip, `win-vulkan-x64`),
		llamaAsset("windows", "amd64", "rocm", ArchiveZip, `win-rocm-([0-9.]+)-x64`),
		llamaAsset("windows", "amd64", "cpu", ArchiveZip, `win-cpu-x64`),
		llamaAsset("windows", "amd64", "sycl", ArchiveZip, `win-sycl-x64`),
		llamaAsset("windows", "amd64", "cuda", ArchiveZip, `win-cuda-([0-9.]+)-x64`),
		llamaAsset("windows", "arm64", "vulkan", ArchiveZip, `win-vulkan-arm64`),
		llamaAsset("windows", "arm64", "cpu", ArchiveZip, `win-cpu-arm64`),
		llamaAsset("windows", "arm64", "cuda", ArchiveZip, `win-cuda-([0-9.]+)-arm64`),
	},
	StripPrefix: "llama-%s/",
	MainBinary:  "llama-server",
	LinkedBinaries: []string{
		"llama-server",
		"llama",
		"llama-cli",
		"llama-completion",
		"llama-bench",
		"llama-batched-bench",
		"llama-connect",
		"llama-fit-params",
		"llama-gemma3-cli",
		"llama-gguf-split",
		"llama-imatrix",
		"llama-llava-cli",
		"llama-minicpmv-cli",
		"llama-mtmd-cli",
		"llama-mtmd-debug",
		"llama-perplexity",
		"llama-quantize",
		"llama-qwen2vl-cli",
		"llama-results",
		"llama-tokenize",
		"llama-tts",
		"ggml-rpc-server",
	},
}
