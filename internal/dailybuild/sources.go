package dailybuild

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
)

// cachedSource remembers successful downloads and failures for this invocation.
type cachedSource struct {
	Filename string
	Err      error
}

// sourceStore shares GitHub discovery and converted RPMs between artifacts and targets.
type sourceStore struct {
	cfg       *config.Config
	client    *http.Client
	directory string
	github    *cachedSource
	release   *releaseSource
	rpms      map[string]cachedSource
	run       externalRun
	out       io.Writer
}

// fetch writes a single artifact under its manifest filename in private staging.
func (s *sourceStore) fetch(ctx context.Context, target string, artifact artifactPlan, destination string) error {
	switch artifact.Source.Type {
	case "static":
		if err := download(ctx, s.client, artifact.URL, destination); err != nil {
			return fmt.Errorf("source %s: %w", artifact.URL, err)
		}
	case "github":
		if s.github == nil {
			s.github = &cachedSource{Filename: filepath.Join(s.directory, "github.zip")}
			g := githubAPI{client: s.client, base: "https://api.github.com", settings: s.cfg.Sources.Dailybuild.GitHub, token: s.cfg.Sources.Dailybuild.GitHub.Token}
			s.github.Err = g.downloadZIP(ctx, s.github.Filename)
		}
		if s.github.Err != nil {
			return s.github.Err
		}
		return extractZIP(ctx, s.github.Filename, artifact.Source.Match, destination)
	case "github_release":
		return s.fetchRelease(ctx, artifact.Source.Match, destination)
	case "rpm":
		base := s.cfg.Targets[target].Dailybuild.OVMFBaseURL
		key := base + "|" + artifact.Source.Package + "|" + artifact.Source.Architecture
		if s.rpms == nil {
			s.rpms = make(map[string]cachedSource)
		}
		cached, ok := s.rpms[key]
		if !ok {
			cached = s.prepareRPM(ctx, base, artifact.Source)
			s.rpms[key] = cached
		}
		if cached.Err != nil {
			return fmt.Errorf("source %s: %w", base, cached.Err)
		}
		return extractCPIO(ctx, cached.Filename, artifact.Source.InternalPath, destination, s.run)
	default:
		return fmt.Errorf("unsupported artifact source %q", artifact.Source.Type)
	}
	return nil
}

// fetchRelease resolves latest only once, then matches each artifact against its assets.
func (s *sourceStore) fetchRelease(ctx context.Context, pattern, destination string) error {
	settings := s.cfg.Sources.Dailybuild.GitHubRelease
	if s.release == nil {
		s.release = &releaseSource{api: githubAPI{
			client: s.client, base: "https://api.github.com",
			settings: config.GitHubSource{Owner: settings.Owner, Repository: settings.Repository},
			token:    settings.Token,
		}}
		s.release.release, s.release.err = s.release.api.latestRelease(ctx)
		if s.release.err == nil {
			s.release.assets, s.release.err = s.release.api.releaseAssets(ctx, s.release.release.ID)
		}
		if s.release.err == nil && s.out != nil {
			fmt.Fprintf(s.out, "Selected GitHub release: %s/%s tag %q (ID %d)\n", settings.Owner, settings.Repository, s.release.release.Tag, s.release.release.ID)
		}
	}
	if s.release.err != nil {
		return fmt.Errorf("GitHub release %s/%s: %w", settings.Owner, settings.Repository, s.release.err)
	}
	asset, err := matchReleaseAsset(s.release.assets, pattern)
	if err != nil {
		return fmt.Errorf("GitHub release %s/%s tag %q: %w", settings.Owner, settings.Repository, s.release.release.Tag, err)
	}
	if s.out != nil {
		fmt.Fprintf(s.out, "  GitHub release asset: %q -> %s\n", asset.Name, destination)
	}
	if err := s.release.api.downloadReleaseAsset(ctx, asset, destination); err != nil {
		return fmt.Errorf("GitHub release %s/%s tag %q asset %q: %w", settings.Owner, settings.Repository, s.release.release.Tag, asset.Name, err)
	}
	return nil
}

// prepareRPM discovers and converts a package once; all work stays in staging.
func (s *sourceStore) prepareRPM(ctx context.Context, base string, source config.ArtifactSource) cachedSource {
	candidate, err := discoverRPM(ctx, s.client, base, source.Package, source.Architecture)
	if err != nil {
		return cachedSource{Err: err}
	}
	directory, err := os.MkdirTemp(s.directory, "rpm-*")
	if err != nil {
		return cachedSource{Err: err}
	}
	rpm := filepath.Join(directory, "package.rpm")
	cpio := filepath.Join(directory, "package.cpio")
	if err := download(ctx, s.client, candidate.URL, rpm); err != nil {
		return cachedSource{Err: fmt.Errorf("download %s: %w", candidate.URL, err)}
	}
	if err := convertRPM(ctx, rpm, cpio, s.run); err != nil {
		return cachedSource{Err: err}
	}
	return cachedSource{Filename: cpio}
}
