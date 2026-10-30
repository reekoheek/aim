package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/reekoheek/aim/pkg/backend"
)

const (
	DefaultGitHubAPIURL   = "https://api.github.com"
	gitHubUserAgent       = "aim-backend"
	gitHubJSONMediaType   = "application/vnd.github+json"
	rateLimitRemainingKey = "X-RateLimit-Remaining"
	errorBodyPreviewLimit = 4096
)

var (
	ErrRateLimited = errors.New("GitHub API rate limit hit; set GITHUB_TOKEN or GH_TOKEN to raise it")
	ErrNotFound    = errors.New("not found on GitHub")
)

type GitHub struct {
	APIURL string
	Token  string
	Client *http.Client
}

func NewGitHub(token string) *GitHub {
	return &GitHub{APIURL: DefaultGitHubAPIURL, Token: token, Client: http.DefaultClient}
}

func TokenFromEnvironment(lookup func(key string) string) string {
	if token := lookup("GITHUB_TOKEN"); token != "" {
		return token
	}
	return lookup("GH_TOKEN")
}

type gitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type gitHubRelease struct {
	TagName     string        `json:"tag_name"`
	PublishedAt time.Time     `json:"published_at"`
	Body        string        `json:"body"`
	Assets      []gitHubAsset `json:"assets"`
}

func (r gitHubRelease) toRelease() backend.Release {
	release := backend.Release{Tag: r.TagName, PublishedAt: r.PublishedAt, Body: r.Body}
	for _, asset := range r.Assets {
		release.Assets = append(release.Assets, backend.Asset{Name: asset.Name, URL: asset.BrowserDownloadURL, Size: asset.Size})
	}
	return release
}

func (g *GitHub) LatestRelease(ctx context.Context, repository string) (backend.Release, error) {
	var release gitHubRelease
	if err := g.getJSON(ctx, "/repos/"+repository+"/releases/latest", &release); err != nil {
		return backend.Release{}, err
	}
	return release.toRelease(), nil
}

func (g *GitHub) ReleaseByTag(ctx context.Context, repository string, tag string) (backend.Release, error) {
	var release gitHubRelease
	if err := g.getJSON(ctx, "/repos/"+repository+"/releases/tags/"+url.PathEscape(tag), &release); err != nil {
		return backend.Release{}, err
	}
	return release.toRelease(), nil
}

func (g *GitHub) Releases(ctx context.Context, repository string, perPage int, page int) ([]backend.Release, error) {
	var releases []gitHubRelease
	path := fmt.Sprintf("/repos/%s/releases?per_page=%d&page=%d", repository, perPage, page)
	if err := g.getJSON(ctx, path, &releases); err != nil {
		return nil, err
	}
	converted := make([]backend.Release, 0, len(releases))
	for _, release := range releases {
		converted = append(converted, release.toRelease())
	}
	return converted, nil
}

func (g *GitHub) Download(ctx context.Context, address string, destination io.Writer) error {
	response, err := g.do(ctx, address, "*/*")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if _, err := io.Copy(destination, response.Body); err != nil {
		return fmt.Errorf("download %s: %w", address, err)
	}
	return nil
}

func (g *GitHub) getJSON(ctx context.Context, path string, target any) error {
	address := strings.TrimRight(g.APIURL, "/") + path
	response, err := g.do(ctx, address, gitHubJSONMediaType)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", address, err)
	}
	return nil
}

func (g *GitHub) do(ctx context.Context, address string, accept string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", gitHubUserAgent)
	if g.Token != "" && strings.HasPrefix(address, g.APIURL) {
		request.Header.Set("Authorization", "Bearer "+g.Token)
	}
	response, err := g.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("network error: %w", err)
	}
	if response.StatusCode == http.StatusOK {
		return response, nil
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyPreviewLimit))
	return nil, statusError(response, body, address)
}

func statusError(response *http.Response, body []byte, address string) error {
	rateLimited := response.Header.Get(rateLimitRemainingKey) == "0" || strings.Contains(strings.ToLower(string(body)), "rate limit")
	switch {
	case (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests) && rateLimited:
		return ErrRateLimited
	case response.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, address)
	default:
		return fmt.Errorf("HTTP %d for %s", response.StatusCode, address)
	}
}
