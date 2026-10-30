package manager

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/reekoheek/aim/pkg/backend"
)

func withRepository(descriptor backend.Descriptor) backend.Descriptor {
	descriptor.Repository = fakeRepository
	return descriptor
}

func llamaFakeReleases() []fakeRelease {
	return []fakeRelease{
		{tag: "b300", assets: map[string][]byte{"llama-b300-bin-ubuntu-vulkan-x64.tar.gz": nil}},
		{tag: "b299", assets: map[string][]byte{"llama-b299-bin-ubuntu-vulkan-x64.tar.gz": nil, "llama-b299-bin-ubuntu-rocm-7.0-x64.tar.gz": nil}},
		{tag: "v1.1.0", assets: map[string][]byte{"nightly-tag.txt": []byte("b299\n")}},
		{tag: "b298", assets: map[string][]byte{"llama-b298-bin-ubuntu-rocm-7.0-x64.tar.gz": nil}},
		{tag: "v1.0.0", body: "binaries: https://github.com/x/y/releases/tag/b297"},
		{tag: "b297"},
		{tag: "v0.9.0"},
	}
}

func TestResolverResolve(t *testing.T) {
	sdReleases := []fakeRelease{{tag: "master-1028-37bc88b"}, {tag: "master-1027-e7b7ec6"}}
	for number := 1026; number > 800; number-- {
		sdReleases = append(sdReleases, fakeRelease{tag: fmt.Sprintf("master-%d-abc%d", number, number)})
	}

	tests := []struct {
		name       string
		descriptor backend.Descriptor
		latest     string
		releases   []fakeRelease
		spec       string
		wantBuild  string
		wantStable string
		wantErr    bool
	}{
		{name: "llama latest via nightly-tag", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "latest", wantBuild: "b299", wantStable: "v1.1.0"},
		{name: "llama stable via body link", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "v1.0", wantBuild: "b297", wantStable: "v1.0.0"},
		{name: "llama stable without build fails", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "v0.9.0", wantErr: true},
		{name: "llama latest that is a build", descriptor: backend.LlamaCpp, latest: "b300", releases: llamaFakeReleases(), spec: "", wantBuild: "b300"},
		{name: "llama dev", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "nightly", wantBuild: "b300"},
		{name: "llama build tag", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "b298", wantBuild: "b298"},
		{name: "llama bare number", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "298", wantBuild: "b298"},
		{name: "llama missing build", descriptor: backend.LlamaCpp, latest: "v1.1.0", releases: llamaFakeReleases(), spec: "b100", wantErr: true},
		{name: "sd latest", descriptor: backend.SDCpp, latest: "master-1028-37bc88b", releases: sdReleases, spec: "latest", wantBuild: "master-1028-37bc88b"},
		{name: "sd dev", descriptor: backend.SDCpp, latest: "master-1028-37bc88b", releases: sdReleases, spec: "dev", wantBuild: "master-1028-37bc88b"},
		{name: "sd full tag", descriptor: backend.SDCpp, latest: "master-1028-37bc88b", releases: sdReleases, spec: "master-1027-e7b7ec6", wantBuild: "master-1027-e7b7ec6"},
		{name: "sd number on a later page", descriptor: backend.SDCpp, latest: "master-1028-37bc88b", releases: sdReleases, spec: "850", wantBuild: "master-850-abc850"},
		{name: "sd unknown number", descriptor: backend.SDCpp, latest: "master-1028-37bc88b", releases: sdReleases, spec: "412", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeGitHub(t, tt.latest, tt.releases)
			descriptor := withRepository(tt.descriptor)
			resolver := Resolver{Descriptor: descriptor, Source: fake.client()}
			version, err := descriptor.ParseVersion(tt.spec)
			if err != nil {
				t.Fatal(err)
			}

			got, err := resolver.Resolve(context.Background(), version)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.Build != tt.wantBuild || got.Stable != tt.wantStable {
				t.Fatalf("Resolve() = (%q, %q), want (%q, %q)", got.Build, got.Stable, tt.wantBuild, tt.wantStable)
			}
			if !tt.wantErr && got.Release.Tag != tt.wantBuild {
				t.Fatalf("Resolve().Release.Tag = %q, want %q", got.Release.Tag, tt.wantBuild)
			}
		})
	}
}

func TestResolverBuildsWithAsset(t *testing.T) {
	tests := []struct {
		name    string
		variant string
		limit   int
		want    []string
	}{
		{name: "rocm skips builds without asset", variant: "rocm", limit: 6, want: []string{"b299", "b298"}},
		{name: "vulkan limited", variant: "vulkan", limit: 1, want: []string{"b300"}},
		{name: "sycl none", variant: "sycl", limit: 6, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeGitHub(t, "v1.1.0", llamaFakeReleases())
			resolver := Resolver{Descriptor: withRepository(backend.LlamaCpp), Source: fake.client()}

			got, err := resolver.BuildsWithAsset(context.Background(), backend.Platform{OS: "linux", Arch: "amd64"}, tt.variant, tt.limit)

			if err != nil {
				t.Fatalf("BuildsWithAsset() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("BuildsWithAsset() = %v, want %v", got, tt.want)
			}
		})
	}
}
