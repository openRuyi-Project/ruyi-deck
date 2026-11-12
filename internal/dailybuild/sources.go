package dailybuild

import (
	"context"
	"fmt"
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

// sourceStore shares one GitHub ZIP and one converted RPM per source between artifacts.
type sourceStore struct {
	cfg       *config.Config
	client    *http.Client
	directory string
	github    *cachedSource
	rpms      map[string]cachedSource
	run       externalRun
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
			g := githubAPI{client: s.client, base: "https://api.github.com", settings: s.cfg.Sources.Dailybuild.GitHub, token: os.Getenv(s.cfg.Sources.Dailybuild.GitHub.TokenEnv)}
			s.github.Err = g.downloadZIP(ctx, s.github.Filename)
		}
		if s.github.Err != nil {
			return s.github.Err
		}
		return extractZIP(ctx, s.github.Filename, artifact.Source.Match, destination)
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
