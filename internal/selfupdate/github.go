package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pilef-co/bqto/ext"
)

const (
	repo           = "pilef-co/bqto"
	maxReleaseBody = 1 << 20
	maxArchive     = 200 << 20
	maxChecksums   = 1 << 16
)

type GitHub struct {
	API      string
	Download string
	Client   *http.Client
	Have     string
}

func DefaultGitHub(have string) GitHub {
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("GitHub redirected to %s, which is not HTTPS", req.URL.Redacted())
		}
		if len(via) >= 10 {
			return errors.New("GitHub redirected more than 10 times")
		}
		return nil
	}}
	return GitHub{API: "https://api.github.com", Download: "https://github.com", Client: client, Have: have}
}

type githubRelease struct {
	Tag  string `json:"tag_name"`
	Body string `json:"body"`
}

var errNoPublicRelease = errors.New("GitHub has no public bqto release to compare with")

func (g GitHub) Latest(ctx context.Context) (ext.Release, error) {
	url := g.API + "/repos/" + repo + "/releases/latest"
	resp, err := g.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return ext.Release{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return ext.Release{}, fmt.Errorf("%w (%s)", errNoPublicRelease, resp.Status)
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0":
		return ext.Release{}, fmt.Errorf("GitHub's rate limit for your network is used up (%s)\nFix: wait an hour and run the command again", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return ext.Release{}, fmt.Errorf("GitHub answered %s for the latest bqto release", resp.Status)
	}
	var rel githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBody)).Decode(&rel); err != nil {
		return ext.Release{}, fmt.Errorf("GitHub sent a latest release bqto cannot read: %w", err)
	}
	if !releaseTag(rel.Tag) {
		return ext.Release{}, fmt.Errorf("GitHub sent the tag %q, which is not vX.Y.Z", rel.Tag)
	}
	newer := ext.NewerVersion(rel.Tag, g.Have)
	return ext.Release{
		Version:  ext.ShortVersion(rel.Tag),
		Newer:    newer,
		Security: newer && fixesSecurity(rel.Body, rel.Tag, g.Have),
		Ref:      rel.Tag,
	}, nil
}

func releaseTag(tag string) bool {
	return strings.HasPrefix(tag, "v") && tag == "v"+ext.ShortVersion(tag)
}

func fixesSecurity(body, tag, have string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "Security") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.EqualFold(value, "yes") {
			value = tag
		}
		if ext.NewerVersion(value, have) {
			return true
		}
	}
	return false
}

func ArchiveName(project, version string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", project, version, runtime.GOOS, runtime.GOARCH)
}

func (g GitHub) Fetch(ctx context.Context, r ext.Release, dir string) (string, error) {
	if !releaseTag(r.Ref) {
		return "", fmt.Errorf("the release tag %q is not vX.Y.Z\nFix: run bqto update again", r.Ref)
	}
	archive := ArchiveName("bqto", ext.ShortVersion(r.Ref))
	base := g.Download + "/" + repo + "/releases/download/" + r.Ref + "/"
	if err := g.save(ctx, base+archive, filepath.Join(dir, archive), maxArchive); err != nil {
		return "", err
	}
	if err := g.save(ctx, base+"checksums.txt", filepath.Join(dir, "checksums.txt"), maxChecksums); err != nil {
		return "", err
	}
	return filepath.Join(dir, archive), nil
}

func (g GitHub) save(ctx context.Context, url, path string, limit int64) error {
	resp, err := g.get(ctx, url, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub answered %s for %s\nFix: download the release by hand, as the README shows under Build and install", resp.Status, filepath.Base(path))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // path is a fixed name in bqto's own temporary folder
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("downloading %s: %w\nFix: check your network and run bqto update again", filepath.Base(path), err)
	}
	if n > limit {
		return fmt.Errorf("%s is larger than %d bytes, so bqto did not use it\nFix: download the release by hand, as the README shows under Build and install", filepath.Base(path), limit)
	}
	return nil
}

func (g GitHub) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "bqto")
	client := g.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not reach GitHub: %w", err)
	}
	return resp, nil
}
