package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeRepository = "owner/repo"

type fakeRelease struct {
	tag    string
	body   string
	assets map[string][]byte
}

type fakeGitHub struct {
	server     *httptest.Server
	latest     string
	releases   []fakeRelease
	rateLimit  bool
	mu         sync.Mutex
	authHeader []string
	downloads  []string
}

func newFakeGitHub(t *testing.T, latest string, releases []fakeRelease) *fakeGitHub {
	t.Helper()
	fake := &fakeGitHub{latest: latest, releases: releases}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeGitHub) client() *GitHub {
	return &GitHub{APIURL: f.server.URL, Token: "secret", Client: f.server.Client()}
}

func (f *fakeGitHub) downloadURL(tag string, name string) string {
	return f.server.URL + "/download/" + tag + "/" + name
}

func (f *fakeGitHub) toJSON(release fakeRelease, index int) gitHubRelease {
	converted := gitHubRelease{
		TagName:     release.tag,
		Body:        release.body,
		PublishedAt: time.Date(2026, 10, 30-index, 0, 0, 0, 0, time.UTC),
	}
	for name, content := range release.assets {
		converted.Assets = append(converted.Assets, gitHubAsset{Name: name, BrowserDownloadURL: f.downloadURL(release.tag, name), Size: int64(len(content))})
	}
	return converted
}

func (f *fakeGitHub) find(tag string) (fakeRelease, int, bool) {
	for index, release := range f.releases {
		if release.tag == tag {
			return release, index, true
		}
	}
	return fakeRelease{}, 0, false
}

func (f *fakeGitHub) serve(writer http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	f.authHeader = append(f.authHeader, request.Header.Get("Authorization"))
	f.mu.Unlock()
	if f.rateLimit {
		writer.Header().Set(rateLimitRemainingKey, "0")
		http.Error(writer, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
		return
	}
	prefix := "/repos/" + fakeRepository + "/releases"
	path := request.URL.Path
	switch {
	case strings.HasPrefix(path, "/download/"):
		f.serveDownload(writer, strings.TrimPrefix(path, "/download/"))
	case path == prefix+"/latest":
		f.serveTag(writer, f.latest)
	case strings.HasPrefix(path, prefix+"/tags/"):
		f.serveTag(writer, strings.TrimPrefix(path, prefix+"/tags/"))
	case path == prefix:
		f.servePage(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (f *fakeGitHub) serveDownload(writer http.ResponseWriter, tagAndName string) {
	tag, name, _ := strings.Cut(tagAndName, "/")
	f.mu.Lock()
	f.downloads = append(f.downloads, name)
	f.mu.Unlock()
	release, _, ok := f.find(tag)
	content, found := release.assets[name]
	if !ok || !found {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	_, _ = writer.Write(content)
}

func (f *fakeGitHub) serveTag(writer http.ResponseWriter, tag string) {
	release, index, ok := f.find(tag)
	if !ok {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(writer).Encode(f.toJSON(release, index))
}

func (f *fakeGitHub) servePage(writer http.ResponseWriter, request *http.Request) {
	perPage, _ := strconv.Atoi(request.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(request.URL.Query().Get("page"))
	start := (page - 1) * perPage
	converted := []gitHubRelease{}
	for index := start; index < len(f.releases) && index < start+perPage; index++ {
		converted = append(converted, f.toJSON(f.releases[index], index))
	}
	_ = json.NewEncoder(writer).Encode(converted)
}

func TestGitHubReleases(t *testing.T) {
	fake := newFakeGitHub(t, "v1.0.0", []fakeRelease{
		{tag: "b3", assets: map[string][]byte{"a.tar.gz": []byte("three")}},
		{tag: "v1.0.0", body: "see releases/tag/b2"},
		{tag: "b2"},
	})
	client := fake.client()
	ctx := context.Background()

	tests := []struct {
		name    string
		call    func() (string, error)
		want    string
		wantErr error
	}{
		{
			name: "latest",
			call: func() (string, error) {
				release, err := client.LatestRelease(ctx, fakeRepository)
				return release.Tag + "|" + release.Body, err
			},
			want: "v1.0.0|see releases/tag/b2",
		},
		{
			name: "by tag with assets",
			call: func() (string, error) {
				release, err := client.ReleaseByTag(ctx, fakeRepository, "b3")
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%s|%s|%d", release.Tag, release.Assets[0].Name, release.Assets[0].Size), nil
			},
			want: "b3|a.tar.gz|5",
		},
		{
			name: "missing tag",
			call: func() (string, error) {
				_, err := client.ReleaseByTag(ctx, fakeRepository, "b9")
				return "", err
			},
			wantErr: ErrNotFound,
		},
		{
			name: "second page",
			call: func() (string, error) {
				releases, err := client.Releases(ctx, fakeRepository, 2, 2)
				if err != nil {
					return "", err
				}
				var tags []string
				for _, release := range releases {
					tags = append(tags, release.Tag)
				}
				return strings.Join(tags, ","), nil
			},
			want: "b2",
		},
		{
			name: "download",
			call: func() (string, error) {
				var content bytes.Buffer
				err := client.Download(ctx, fake.downloadURL("b3", "a.tar.gz"), &content)
				return content.String(), err
			},
			want: "three",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.call()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	for _, header := range fake.authHeader {
		if header != "Bearer secret" {
			t.Fatalf("Authorization = %q, want bearer token on every API request", header)
		}
	}
}

func TestGitHubTokenOnlyForAPI(t *testing.T) {
	fake := newFakeGitHub(t, "", []fakeRelease{{tag: "b1", assets: map[string][]byte{"x": []byte("x")}}})
	client := fake.client()
	client.APIURL = "https://api.github.invalid"

	if err := client.Download(context.Background(), fake.downloadURL("b1", "x"), &bytes.Buffer{}); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if fake.authHeader[0] != "" {
		t.Fatalf("Authorization = %q, want none for non-API hosts", fake.authHeader[0])
	}
}

func TestGitHubRateLimit(t *testing.T) {
	fake := newFakeGitHub(t, "b1", []fakeRelease{{tag: "b1"}})
	fake.rateLimit = true

	_, err := fake.client().LatestRelease(context.Background(), fakeRepository)

	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want %v", err, ErrRateLimited)
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error %q does not mention GITHUB_TOKEN", err)
	}
}

func TestStatusError(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		remaining string
		body      string
		wantErr   error
		wantText  string
	}{
		{name: "403 rate limit body", status: http.StatusForbidden, body: "API rate limit exceeded", wantErr: ErrRateLimited},
		{name: "429 remaining zero", status: http.StatusTooManyRequests, remaining: "0", wantErr: ErrRateLimited},
		{name: "403 other", status: http.StatusForbidden, body: "forbidden", wantText: "HTTP 403"},
		{name: "404", status: http.StatusNotFound, wantErr: ErrNotFound},
		{name: "500", status: http.StatusInternalServerError, wantText: "HTTP 500"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			if tt.remaining != "" {
				response.Header.Set(rateLimitRemainingKey, tt.remaining)
			}
			err := statusError(response, []byte(tt.body), "https://example.test")
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantText)
			}
		})
	}
}

func TestTokenFromEnvironment(t *testing.T) {
	tests := []struct {
		name        string
		environment map[string]string
		want        string
	}{
		{name: "github token", environment: map[string]string{"GITHUB_TOKEN": "a", "GH_TOKEN": "b"}, want: "a"},
		{name: "gh token fallback", environment: map[string]string{"GH_TOKEN": "b"}, want: "b"},
		{name: "none", environment: map[string]string{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TokenFromEnvironment(func(key string) string { return tt.environment[key] })
			if got != tt.want {
				t.Fatalf("TokenFromEnvironment() = %q, want %q", got, tt.want)
			}
		})
	}
}
