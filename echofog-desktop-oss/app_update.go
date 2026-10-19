package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// version is the app's release, set at build time: -ldflags "-X main.version=0.1.1". Builds
// from a branch carry "0.0.0-<commit>" and local builds this default.
var version = "0.1.0"

// latestReleaseURL is GitHub's public API for the newest published release (drafts and
// pre-releases are not returned). It needs no key; GitHub allows 60 requests an hour per IP.
var latestReleaseURL = "https://api.github.com/repos/AdmiralCore/EchoFog/releases/latest"

// UpdateInfo says whether a newer release is out.
type UpdateInfo struct {
	Current string `json:"current"`
	Latest  string `json:"latest,omitempty"`
	URL     string `json:"url,omitempty"` // the release page to download from
	Newer   bool   `json:"newer"`
}

// AppVersion is the version of this build.
func (a *App) AppVersion() string { return version }

// CheckForUpdate asks GitHub for the newest release once and compares it with this build. It
// sends nothing about the user: an anonymous GET of a public page, like opening it in a browser.
// Branch builds ("0.0.0-…") do not ask.
func (a *App) CheckForUpdate() (UpdateInfo, error) {
	info := UpdateInfo{Current: version}
	if strings.HasPrefix(version, "0.0.0") {
		return info, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tag, url, err := fetchLatestRelease(ctx, http.DefaultClient, latestReleaseURL)
	if err != nil {
		return info, err
	}
	info.Latest, info.URL = strings.TrimPrefix(tag, "v"), url
	info.Newer = versionNewer(info.Latest, version)
	return info, nil
}

func fetchLatestRelease(ctx context.Context, c *http.Client, url string) (tag, page string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "EchoFog-desktop") // GitHub refuses requests without one
	resp, err := c.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", errors.New("no release published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", errors.New("GitHub answered " + resp.Status)
	}
	var r struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return "", "", err
	}
	if r.TagName == "" {
		return "", "", errors.New("release without a tag")
	}
	if !strings.HasPrefix(r.HTMLURL, "https://github.com/") {
		r.HTMLURL = "https://github.com/AdmiralCore/EchoFog/releases/latest"
	}
	return r.TagName, r.HTMLURL, nil
}

// versionNewer reports whether a is a later release than b ("1.2.10" > "1.2.9"). A version
// with a pre-release suffix ("1.3.0-rc1") counts as older than the same version without one.
func versionNewer(a, b string) bool {
	pa, sa := splitVersion(a)
	pb, sb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return sa == "" && sb != ""
}

func splitVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	suffix := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v, suffix = v[:i], v[i+1:]
	}
	var out [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(part)
		out[i] = n
	}
	return out, suffix
}
