package backend

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ArchiveFormat string

const (
	ArchiveTarGz ArchiveFormat = "tar.gz"
	ArchiveZip   ArchiveFormat = "zip"
)

const windowsExecutableSuffix = ".exe"

type Asset struct {
	Name   string
	URL    string
	Size   int64
	Format ArchiveFormat
}

type Release struct {
	Tag         string
	PublishedAt time.Time
	Body        string
	Assets      []Asset
}

func (r Release) Asset(name string) (Asset, bool) {
	for _, asset := range r.Assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return Asset{}, false
}

type Platform struct {
	OS   string
	Arch string
}

type VersionKind int

const (
	VersionLatest VersionKind = iota
	VersionDev
	VersionStable
	VersionBuild
)

type Version struct {
	Kind   VersionKind
	Tag    string
	Number int
}

type AssetPattern struct {
	OS      string
	Arch    string
	Variant string
	Format  ArchiveFormat
	Name    *regexp.Regexp
}

type Descriptor struct {
	Name               string
	Repository         string
	BuildTag           *regexp.Regexp
	BuildTagFormat     string
	StableTag          *regexp.Regexp
	StableBuildAsset   string
	LegacyFolderPrefix string
	Variants           []string
	DefaultVariant     string
	DefaultVariantByOS map[string]string
	Assets             []AssetPattern
	StripPrefix        string
	MainBinary         string
	LinkedBinaries     []string
}

var (
	latestSpecs      = map[string]bool{"": true, "latest": true, "stable": true}
	devSpecs         = map[string]bool{"dev": true, "nightly": true, "edge": true, "master": true, "latest-dev": true}
	looseStableSpec  = regexp.MustCompile(`^v?\d+\.\d+(\.\d+)?$`)
	buildNumberSpec  = regexp.MustCompile(`^\d{3,}$`)
	toolkitDigitSpec = regexp.MustCompile(`\d+`)
)

func (d Descriptor) SupportsStable() bool {
	return d.StableTag != nil
}

func (d Descriptor) ParseVersion(spec string) (Version, error) {
	trimmed := strings.TrimSpace(spec)
	if latestSpecs[trimmed] {
		if d.SupportsStable() {
			return Version{Kind: VersionLatest}, nil
		}
		return Version{Kind: VersionDev}, nil
	}
	if devSpecs[trimmed] {
		return Version{Kind: VersionDev}, nil
	}
	if d.BuildTag.MatchString(trimmed) {
		number, _ := d.BuildNumber(trimmed)
		return Version{Kind: VersionBuild, Tag: trimmed, Number: number}, nil
	}
	if d.SupportsStable() && looseStableSpec.MatchString(trimmed) {
		return Version{Kind: VersionStable, Tag: normalizeStableTag(trimmed)}, nil
	}
	if buildNumberSpec.MatchString(trimmed) {
		number, _ := strconv.Atoi(trimmed)
		version := Version{Kind: VersionBuild, Number: number}
		if d.BuildTagFormat != "" {
			version.Tag = fmt.Sprintf(d.BuildTagFormat, number)
		}
		return version, nil
	}
	return Version{}, fmt.Errorf("not a %s version: %q", d.Name, spec)
}

func normalizeStableTag(spec string) string {
	tag := "v" + strings.TrimPrefix(spec, "v")
	if strings.Count(tag, ".") == 1 {
		tag += ".0"
	}
	return tag
}

func (d Descriptor) IsBuildTag(tag string) bool {
	return d.BuildTag.MatchString(tag)
}

func (d Descriptor) IsStableTag(tag string) bool {
	return d.SupportsStable() && d.StableTag.MatchString(tag)
}

func (d Descriptor) BuildNumber(tag string) (int, bool) {
	match := d.BuildTag.FindStringSubmatch(tag)
	if len(match) < 2 {
		return 0, false
	}
	number, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	return number, true
}

func (d Descriptor) HasVariant(variant string) bool {
	for _, known := range d.Variants {
		if known == variant {
			return true
		}
	}
	return false
}

func (d Descriptor) DefaultVariantFor(platform Platform) string {
	if variant, ok := d.DefaultVariantByOS[platform.OS]; ok && d.supportsVariant(platform, variant) {
		return variant
	}
	if d.supportsVariant(platform, d.DefaultVariant) {
		return d.DefaultVariant
	}
	for _, pattern := range d.Assets {
		if pattern.OS == platform.OS && pattern.Arch == platform.Arch {
			return pattern.Variant
		}
	}
	return d.DefaultVariant
}

func (d Descriptor) supportsVariant(platform Platform, variant string) bool {
	for _, pattern := range d.Assets {
		if pattern.OS == platform.OS && pattern.Arch == platform.Arch && pattern.Variant == variant {
			return true
		}
	}
	return false
}

func (d Descriptor) PickAsset(release Release, platform Platform, variant string) (Asset, bool) {
	var best Asset
	var bestToolkit []int
	found := false
	for _, pattern := range d.Assets {
		if pattern.OS != platform.OS || pattern.Arch != platform.Arch || pattern.Variant != variant {
			continue
		}
		for _, asset := range release.Assets {
			match := pattern.Name.FindStringSubmatch(asset.Name)
			if match == nil {
				continue
			}
			toolkit := toolkitVersion(match)
			if !found || compareToolkits(toolkit, bestToolkit) > 0 {
				best = asset
				best.Format = pattern.Format
				bestToolkit = toolkit
				found = true
			}
		}
	}
	return best, found
}

func toolkitVersion(match []string) []int {
	if len(match) < 2 {
		return nil
	}
	var parts []int
	for _, digits := range toolkitDigitSpec.FindAllString(match[1], -1) {
		number, _ := strconv.Atoi(digits)
		parts = append(parts, number)
	}
	return parts
}

func compareToolkits(left []int, right []int) int {
	for index := 0; index < len(left) || index < len(right); index++ {
		var leftPart, rightPart int
		if index < len(left) {
			leftPart = left[index]
		}
		if index < len(right) {
			rightPart = right[index]
		}
		if leftPart != rightPart {
			if leftPart > rightPart {
				return 1
			}
			return -1
		}
	}
	return 0
}

func (d Descriptor) FolderName(build string, variant string) string {
	return build + "-" + variant
}

func (d Descriptor) ParseFolderName(name string) (string, string, bool) {
	separator := strings.LastIndex(name, "-")
	if separator <= 0 {
		return "", "", false
	}
	build := name[:separator]
	variant := name[separator+1:]
	if d.LegacyFolderPrefix != "" {
		build = strings.TrimPrefix(build, d.LegacyFolderPrefix)
	}
	if !d.HasVariant(variant) || !d.IsBuildTag(build) {
		return "", "", false
	}
	return build, variant, true
}

func (d Descriptor) StripPrefixFor(build string) string {
	if d.StripPrefix == "" {
		return ""
	}
	return fmt.Sprintf(d.StripPrefix, build)
}

func ExecutableName(name string, operatingSystem string) string {
	if operatingSystem == "windows" {
		return name + windowsExecutableSuffix
	}
	return name
}

var registry = map[string]Descriptor{
	LlamaCpp.Name: LlamaCpp,
	SDCpp.Name:    SDCpp,
}

func Lookup(name string) (Descriptor, bool) {
	descriptor, ok := registry[name]
	return descriptor, ok
}

func Names() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
