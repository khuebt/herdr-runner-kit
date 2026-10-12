// Package upgrade finds herdr-runner-kit releases on GitHub and swaps the
// installed binary for a verified one.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// Repo is the GitHub repository that publishes the releases.
const Repo = "khuebt/herdr-runner-kit"

// maxDownload bounds every download; a release archive is a few MiB.
const maxDownload = 200 << 20

// Client talks to the GitHub Releases API over HTTPS.
type Client struct {
	API   string // API base URL, https://api.github.com
	HTTP  *http.Client
	Token string // optional GITHUB_TOKEN, sent to the API only, for its higher rate limit
}

// New returns a client for api.github.com that honours GITHUB_TOKEN.
func New() *Client {
	return &Client{API: "https://api.github.com", HTTP: &http.Client{Timeout: 2 * time.Minute}, Token: os.Getenv("GITHUB_TOKEN")}
}

// Release is one published release.
type Release struct {
	Tag        string  `json:"tag_name"`
	Body       string  `json:"body"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// Latest returns the newest stable release.
func (c *Client) Latest(ctx context.Context) (Release, error) {
	var r Release
	return r, c.api(ctx, "/repos/"+Repo+"/releases/latest", &r)
}

// Tag returns the release with the given tag, such as v0.2.0.
func (c *Client) Tag(ctx context.Context, tag string) (Release, error) {
	var r Release
	return r, c.api(ctx, "/repos/"+Repo+"/releases/tags/"+url.PathEscape(tag), &r)
}

// List returns the most recent releases, newest first.
func (c *Client) List(ctx context.Context) ([]Release, error) {
	var rs []Release
	return rs, c.api(ctx, "/repos/"+Repo+"/releases?per_page=100", &rs)
}

func (c *Client) api(ctx context.Context, path string, v any) error {
	req, err := c.request(ctx, c.API+path)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	body, err := c.do(req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GitHub API %s: %w", path, err)
	}
	return nil
}

func (c *Client) download(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := c.request(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) request(ctx context.Context, rawURL string) (*http.Request, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("refusing %s: downloads need HTTPS", rawURL)
	}
	return http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
}

func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: not found", req.URL.Path)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", req.URL.Path, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("%s: larger than %d MiB", req.URL.Path, maxDownload>>20)
	}
	return data, nil
}

type latestCache struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest"`
}

// CachePath is where the newest known release tag is kept between checks.
func CachePath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "herdr-runner-kit", "latest.json"), nil
}

// LatestCached returns the newest release tag, asking GitHub only when the
// cached answer is older than maxAge.
func (c *Client) LatestCached(ctx context.Context, path string, maxAge time.Duration) (string, error) {
	var cached latestCache
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &cached) == nil &&
		cached.Latest != "" && time.Since(cached.CheckedAt) < maxAge {
		return cached.Latest, nil
	}
	rel, err := c.Latest(ctx)
	if err != nil {
		return "", err
	}
	if rel.Tag == "" {
		return "", errors.New("GitHub returned a release without a tag")
	}
	data, err := json.Marshal(latestCache{CheckedAt: time.Now().UTC(), Latest: rel.Tag})
	if err == nil && os.MkdirAll(filepath.Dir(path), 0o755) == nil {
		_ = fsutil.WriteFileAtomic(path, data, 0o644) // a lost cache only costs one more request
	}
	return rel.Tag, nil
}
