package dailybuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

// githubRelease pins discovery to one published release for this invocation.
type githubRelease struct {
	ID         int64  `json:"id"`
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// githubReleaseAsset contains only the metadata used for matching and validation.
type githubReleaseAsset struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest"`
	UpdatedAt time.Time `json:"updated_at"`
}

// releaseSource caches the selected release, its complete asset list, or an error.
type releaseSource struct {
	api     githubAPI
	release githubRelease
	assets  []githubReleaseAsset
	err     error
}

// latestRelease uses GitHub's latest-release selection, independent of --date.
func (g *githubAPI) latestRelease(ctx context.Context) (githubRelease, error) {
	var release githubRelease
	if _, err := g.jsonPage(ctx, g.repositoryPath()+"/releases/latest", &release); err != nil {
		return release, err
	}
	if release.ID <= 0 || release.Tag == "" || release.Draft || release.Prerelease {
		return release, fmt.Errorf("latest release must be a published release with an ID and tag")
	}
	return release, nil
}

// releaseAssets reads every page for the fixed release ID, never rechecking latest.
func (g *githubAPI) releaseAssets(ctx context.Context, releaseID int64) ([]githubReleaseAsset, error) {
	var assets []githubReleaseAsset
	for page := 1; page <= 100; page++ {
		endpoint := fmt.Sprintf("%s/releases/%d/assets?per_page=100&page=%d", g.repositoryPath(), releaseID, page)
		var batch []githubReleaseAsset
		next, err := g.jsonPage(ctx, endpoint, &batch)
		if err != nil {
			return nil, err
		}
		assets = append(assets, batch...)
		if !next {
			return assets, nil
		}
	}
	return nil, fmt.Errorf("GitHub release asset listing exceeds the pagination limit")
}

// matchReleaseAsset requires exactly one complete filename match, including extension.
func matchReleaseAsset(assets []githubReleaseAsset, pattern string) (githubReleaseAsset, error) {
	var matches []githubReleaseAsset
	for _, asset := range assets {
		matched, err := path.Match(pattern, asset.Name)
		if err != nil {
			return githubReleaseAsset{}, err
		}
		if matched {
			matches = append(matches, asset)
		}
	}
	if len(matches) != 1 {
		return githubReleaseAsset{}, fmt.Errorf("release asset pattern %q matched %d files; expected exactly one", pattern, len(matches))
	}
	asset := matches[0]
	if strings.ContainsAny(asset.Name, "/\\\x00\r\n") || asset.Name == "." || asset.Name == ".." {
		return githubReleaseAsset{}, fmt.Errorf("unsafe release asset filename")
	}
	if asset.ID <= 0 || asset.State != "uploaded" || asset.Size <= 0 {
		return githubReleaseAsset{}, fmt.Errorf("release asset %q is incomplete or has invalid metadata", asset.Name)
	}
	return asset, nil
}

// downloadReleaseAsset uses the asset ID and keeps tokens in API request headers.
func (g *githubAPI) downloadReleaseAsset(ctx context.Context, asset githubReleaseAsset, target string) error {
	integrity := downloadIntegrity{Size: asset.Size}
	if asset.Digest != "" {
		algorithm, digest, ok := strings.Cut(asset.Digest, ":")
		if !ok || algorithm != "sha256" {
			return fmt.Errorf("release asset %q has an unsupported digest", asset.Name)
		}
		var err error
		integrity.SHA256, err = hex.DecodeString(digest)
		if err != nil || len(integrity.SHA256) != sha256.Size {
			return fmt.Errorf("release asset %q has an invalid SHA256 digest", asset.Name)
		}
	}
	r, err := g.request(ctx, fmt.Sprintf("%s/releases/assets/%d", g.repositoryPath(), asset.ID))
	if err != nil {
		return err
	}
	r.Header.Set("Accept", "application/octet-stream")
	r.Header.Set("Accept-Encoding", "identity")
	if err := downloadVerifiedRequest(ctx, g.client, r, target, integrity); err != nil {
		return err
	}
	if !asset.UpdatedAt.IsZero() {
		if err := os.Chtimes(target, asset.UpdatedAt, asset.UpdatedAt); err != nil {
			return fmt.Errorf("set release asset timestamp: %w", err)
		}
	}
	return nil
}
