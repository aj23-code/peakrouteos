package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Where releases come from (docs/updates.md, section 5): the GitHub releases of one repository.

// APIBase is where GitHub's API is.
const APIBase = "https://api.github.com"

var (
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	assetRe = regexp.MustCompile(`^peakroute-os-.+\.update$`)
	sha256R = regexp.MustCompile(`(?i)\b([0-9a-f]{64})\b`)
)

// ValidRepo says whether s is written like "owner/name".
func ValidRepo(s string) bool { return repoRe.MatchString(s) }

// Release is a release a device could be updated to.
type Release struct {
	Version     string    `json:"version"` // the tag, such as v0.10.0
	Name        string    `json:"name,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	Prerelease  bool      `json:"prerelease"`
	Published   time.Time `json:"published"`
	AssetURL    string    `json:"asset_url"`
	AssetName   string    `json:"asset_name"`
	Size        int64     `json:"size"`
	ChecksumURL string    `json:"checksum_url,omitempty"` // the .sha256 file next to the asset
	Checksum    string    `json:"checksum,omitempty"`     // or the digest GitHub states for the asset
	Newer       bool      `json:"newer"`                  // newer than the version that is running
}

type ghAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	Tag        string    `json:"tag_name"`
	Name       string    `json:"name"`
	Body       string    `json:"body"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []ghAsset `json:"assets"`
}

func (s *Service) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}}
}

// urlOK: a release's links must be https (plain http only where the tests say so).
func (s *Service) urlOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && s.AllowHTTP)
}

func (s *Service) get(ctx context.Context, raw string, accept string) (*http.Response, error) {
	if !s.urlOK(raw) {
		return nil, fmt.Errorf("%q is not a secure address", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PeakRoute-OS/"+s.version())
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return s.client().Do(req)
}

// FindRelease asks GitHub for the releases of repo and returns the newest one that has an update file and a checksum. A pre-release
// only counts when prerelease is true. It is the newest by version number, not by date. running is the version now running (it sets
// Release.Newer). If no release has an update file, the error says so.
func (s *Service) FindRelease(ctx context.Context, repo string, prerelease bool, running string) (*Release, error) {
	if !ValidRepo(repo) {
		return nil, errors.New("the update source must be written owner/name, such as peakroute/peakroute-os")
	}
	base := s.APIBase
	if base == "" {
		base = APIBase
	}
	resp, err := s.get(ctx, base+"/repos/"+repo+"/releases?per_page=30", "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("GitHub could not be reached: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("GitHub has no repository %s (or it is private)", repo)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, errors.New("GitHub is limiting requests from this address: try again later")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var list []ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&list); err != nil {
		return nil, errors.New("GitHub's answer could not be read")
	}
	runningV, haveRunning := ParseVersion(running)

	type cand struct {
		v Version
		r Release
	}
	var found []cand
	for _, gr := range list {
		if gr.Draft || (gr.Prerelease && !prerelease) {
			continue
		}
		v, ok := ParseVersion(gr.Tag)
		if !ok {
			continue
		}
		var asset *ghAsset
		for i := range gr.Assets {
			if assetRe.MatchString(gr.Assets[i].Name) {
				asset = &gr.Assets[i]
				break
			}
		}
		if asset == nil || !s.urlOK(asset.URL) {
			continue
		}
		r := Release{Version: gr.Tag, Name: gr.Name, Notes: strings.TrimSpace(gr.Body), Prerelease: gr.Prerelease, Published: gr.Published,
			AssetURL: asset.URL, AssetName: asset.Name, Size: asset.Size}
		for _, a := range gr.Assets {
			if a.Name == asset.Name+".sha256" && s.urlOK(a.URL) {
				r.ChecksumURL = a.URL
			}
		}
		if h, ok := strings.CutPrefix(asset.Digest, "sha256:"); ok && hex64.MatchString(strings.ToLower(h)) {
			r.Checksum = strings.ToLower(h)
		}
		if r.ChecksumURL == "" && r.Checksum == "" {
			continue // an update with nothing to check it against is not offered
		}
		if len(r.Notes) > 4000 {
			r.Notes = r.Notes[:4000] + "…"
		}
		r.Newer = !haveRunning || v.Compare(runningV) > 0
		found = append(found, cand{v, r})
	}
	if len(found) == 0 {
		return nil, errors.New("no release of " + repo + " has an update file with a checksum")
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].v.Compare(found[j].v) > 0 })
	r := found[0].r
	return &r, nil
}

// fetchChecksum returns the checksum the release states for its update file.
func (s *Service) fetchChecksum(ctx context.Context, r *Release) (string, error) {
	if r.ChecksumURL == "" {
		if r.Checksum == "" {
			return "", errors.New("this release has no checksum")
		}
		return r.Checksum, nil
	}
	resp, err := s.get(ctx, r.ChecksumURL, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("the checksum could not be downloaded: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the checksum could not be downloaded (%s)", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	// "sha256sum" writes "<hash>  <file name>"
	m := sha256R.FindSubmatch(raw)
	if m == nil {
		return "", errors.New("the checksum file of the release cannot be read")
	}
	sumFromFile := strings.ToLower(string(m[1]))
	if r.Checksum != "" && r.Checksum != sumFromFile {
		return "", errors.New("the checksum file of the release disagrees with the one GitHub states for the update file")
	}
	return sumFromFile, nil
}
