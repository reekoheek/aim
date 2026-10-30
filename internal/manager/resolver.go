package manager

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/reekoheek/aim/pkg/backend"
)

const (
	devScanSize          = 30
	suggestionScanSize   = 40
	releasePageSize      = 100
	buildNumberScanPages = 10
)

var releaseTagLink = regexp.MustCompile(`releases/tag/([A-Za-z0-9._-]+)`)

type ReleaseSource interface {
	LatestRelease(ctx context.Context, repository string) (backend.Release, error)
	ReleaseByTag(ctx context.Context, repository string, tag string) (backend.Release, error)
	Releases(ctx context.Context, repository string, perPage int, page int) ([]backend.Release, error)
	Download(ctx context.Context, address string, destination io.Writer) error
}

type Resolution struct {
	Build   string
	Stable  string
	Release backend.Release
}

type Resolver struct {
	Descriptor backend.Descriptor
	Source     ReleaseSource
}

func (r Resolver) Resolve(ctx context.Context, version backend.Version) (Resolution, error) {
	resolution, err := r.resolveTags(ctx, version)
	if err != nil {
		return Resolution{}, err
	}
	release, err := r.Source.ReleaseByTag(ctx, r.Descriptor.Repository, resolution.Build)
	if err != nil {
		return Resolution{}, err
	}
	resolution.Release = release
	return resolution, nil
}

func (r Resolver) resolveTags(ctx context.Context, version backend.Version) (Resolution, error) {
	switch version.Kind {
	case backend.VersionLatest:
		release, err := r.Source.LatestRelease(ctx, r.Descriptor.Repository)
		if err != nil {
			return Resolution{}, err
		}
		if r.Descriptor.IsBuildTag(release.Tag) {
			return Resolution{Build: release.Tag}, nil
		}
		return r.resolveStable(ctx, release)
	case backend.VersionStable:
		release, err := r.Source.ReleaseByTag(ctx, r.Descriptor.Repository, version.Tag)
		if err != nil {
			return Resolution{}, err
		}
		return r.resolveStable(ctx, release)
	case backend.VersionDev:
		return r.resolveDev(ctx)
	default:
		if version.Tag != "" {
			return Resolution{Build: version.Tag}, nil
		}
		return r.resolveBuildNumber(ctx, version.Number)
	}
}

func (r Resolver) resolveStable(ctx context.Context, release backend.Release) (Resolution, error) {
	build, err := r.StableBuild(ctx, release)
	if err != nil {
		return Resolution{}, err
	}
	if build == "" {
		return Resolution{}, fmt.Errorf("stable release %s names no build; try `dev` or a build tag", release.Tag)
	}
	return Resolution{Build: build, Stable: release.Tag}, nil
}

func (r Resolver) StableBuild(ctx context.Context, release backend.Release) (string, error) {
	if asset, ok := release.Asset(r.Descriptor.StableBuildAsset); ok && r.Descriptor.StableBuildAsset != "" {
		var content bytes.Buffer
		if err := r.Source.Download(ctx, asset.URL, &content); err != nil {
			return "", err
		}
		if build := strings.TrimSpace(content.String()); r.Descriptor.IsBuildTag(build) {
			return build, nil
		}
	}
	for _, match := range releaseTagLink.FindAllStringSubmatch(release.Body, -1) {
		if r.Descriptor.IsBuildTag(match[1]) {
			return match[1], nil
		}
	}
	return "", nil
}

func (r Resolver) resolveDev(ctx context.Context) (Resolution, error) {
	releases, err := r.Source.Releases(ctx, r.Descriptor.Repository, devScanSize, 1)
	if err != nil {
		return Resolution{}, err
	}
	for _, release := range releases {
		if r.Descriptor.IsBuildTag(release.Tag) {
			return Resolution{Build: release.Tag}, nil
		}
	}
	return Resolution{}, fmt.Errorf("no build tag found in the %d newest %s releases", devScanSize, r.Descriptor.Name)
}

func (r Resolver) resolveBuildNumber(ctx context.Context, number int) (Resolution, error) {
	for page := 1; page <= buildNumberScanPages; page++ {
		releases, err := r.Source.Releases(ctx, r.Descriptor.Repository, releasePageSize, page)
		if err != nil {
			return Resolution{}, err
		}
		for _, release := range releases {
			if found, ok := r.Descriptor.BuildNumber(release.Tag); ok && found == number {
				return Resolution{Build: release.Tag}, nil
			}
		}
		if len(releases) < releasePageSize {
			break
		}
	}
	return Resolution{}, fmt.Errorf("no %s release with build number %d", r.Descriptor.Name, number)
}

func (r Resolver) BuildsWithAsset(ctx context.Context, platform backend.Platform, variant string, limit int) ([]string, error) {
	releases, err := r.Source.Releases(ctx, r.Descriptor.Repository, suggestionScanSize, 1)
	if err != nil {
		return nil, err
	}
	var builds []string
	for _, release := range releases {
		if len(builds) >= limit {
			break
		}
		if !r.Descriptor.IsBuildTag(release.Tag) {
			continue
		}
		if _, ok := r.Descriptor.PickAsset(release, platform, variant); ok {
			builds = append(builds, release.Tag)
		}
	}
	return builds, nil
}
