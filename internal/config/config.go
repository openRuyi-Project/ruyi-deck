// Package config loads the version 1 configuration and artifact manifest.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Include IANA zones on minimal Linux installations.

	"gopkg.in/yaml.v3"
)

// Config keeps repository and dailybuild settings independent.
type Config struct {
	Version          int               `yaml:"version"`
	Timezone         string            `yaml:"timezone"`
	Paths            Paths             `yaml:"paths"`
	Layout           Layout            `yaml:"layout"`
	Targets          map[string]Target `yaml:"targets"`
	Sources          Sources           `yaml:"sources"`
	Dailybuild       Dailybuild        `yaml:"dailybuild"`
	Retention        Retention         `yaml:"retention"`
	ArtifactManifest string            `yaml:"artifact_manifest"`
	Manifest         Manifest          `yaml:"-"`
	Location         *time.Location    `yaml:"-"`
}

// Paths are absolute roots after configuration loading.
type Paths struct {
	RepoRoot     string `yaml:"repo_root"`
	ReleasesRoot string `yaml:"releases_root"`
	ArchiveRoot  string `yaml:"archive_root"`
	StagingRoot  string `yaml:"staging_root"`
}

// ChannelLayout maps the rolling logical name to a directory.
type ChannelLayout struct {
	Rolling string `yaml:"rolling"`
}

// Layout contains actual directory names for the three managed areas.
type Layout struct {
	Repo     ChannelLayout `yaml:"repo"`
	Releases ChannelLayout `yaml:"releases"`
	Archive  struct {
		RepoRoot string        `yaml:"repo_root"`
		Repo     ChannelLayout `yaml:"repo"`
	} `yaml:"archive"`
}

// Target has global and per-workflow enable switches.
type Target struct {
	Enabled bool `yaml:"enabled"`
	Repo    struct {
		Enabled    bool   `yaml:"enabled"`
		SourcePath string `yaml:"source_path"`
	} `yaml:"repo"`
	Dailybuild struct {
		Enabled     bool   `yaml:"enabled"`
		OVMFBaseURL string `yaml:"ovmf_base_url"`
	} `yaml:"dailybuild"`
}

// Dailybuild configures the link name and human-readable channel.
type Dailybuild struct {
	CurrentLink string `yaml:"current_link"`
	Channel     string `yaml:"channel"`
}

// Sources separates repo URL mappings from image URL mappings.
type Sources struct {
	Repo struct {
		RollingBaseURL string `yaml:"rolling_base_url"`
	} `yaml:"repo"`
	Dailybuild struct {
		BuildBaseURL string       `yaml:"build_base_url"`
		GitHub       GitHubSource `yaml:"github"`
	} `yaml:"dailybuild"`
}

// GitHubSource uses a workflow filename or workflow ID, never a fixed run ID.
type GitHubSource struct {
	Owner        string `yaml:"owner"`
	Repository   string `yaml:"repository"`
	Workflow     string `yaml:"workflow"`
	ArtifactName string `yaml:"artifact_name"`
	TokenEnv     string `yaml:"token_env"`
}

// Retention counts calendar days including today.
type Retention struct {
	DailybuildDays int `yaml:"dailybuild_days"`
}

// Manifest supplies the single artifact list for downloading and presentation.
type Manifest struct {
	Version   int        `yaml:"version"`
	Artifacts []Artifact `yaml:"artifacts"`
}

// Artifact maps applicable targets to independent sources.
type Artifact struct {
	ID          string                    `yaml:"id"`
	Filename    string                    `yaml:"filename"`
	Description string                    `yaml:"description"`
	Required    *bool                     `yaml:"required"`
	Sources     map[string]ArtifactSource `yaml:"sources"`
}

// ArtifactSource describes a static download, ZIP pattern or RPM internal path.
type ArtifactSource struct {
	Type         string `yaml:"type"`
	URL          string `yaml:"url"`
	Path         string `yaml:"path"`
	Match        string `yaml:"match"`
	Package      string `yaml:"package"`
	Architecture string `yaml:"architecture"`
	InternalPath string `yaml:"internal_path"`
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var fixedZone = regexp.MustCompile(`^UTC([+-])([0-9]{2}):([0-9]{2})$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Load rejects unknown fields and resolves relative paths beside the main YAML.
func Load(filename string) (*Config, error) {
	abs, err := filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	c := &Config{Timezone: "UTC+08:00", ArtifactManifest: "dailybuild-artifacts.yaml", Dailybuild: Dailybuild{CurrentLink: "current", Channel: "Creek"}, Retention: Retention{DailybuildDays: 7}}
	c.Layout.Repo.Rolling = "creek"
	c.Layout.Releases.Rolling = "creek"
	c.Layout.Archive.RepoRoot = "anchorage"
	c.Layout.Archive.Repo.Rolling = "creek"
	c.Sources.Dailybuild.GitHub.TokenEnv = "GITHUB_TOKEN"
	if err := readYAML(abs, c); err != nil {
		return nil, err
	}
	base := filepath.Dir(abs)
	for _, p := range []*string{&c.Paths.RepoRoot, &c.Paths.ReleasesRoot, &c.Paths.ArchiveRoot, &c.Paths.StagingRoot} {
		if strings.TrimSpace(*p) == "" || strings.ContainsAny(*p, "\x00\r\n") {
			return nil, errors.New("all four filesystem roots are required and must be valid paths")
		}
		*p = resolvePath(base, *p)
	}
	if c.ArtifactManifest == "" {
		return nil, errors.New("artifact_manifest must name a YAML file")
	}
	c.ArtifactManifest = resolvePath(base, c.ArtifactManifest)
	if err := readYAML(c.ArtifactManifest, &c.Manifest); err != nil {
		return nil, fmt.Errorf("artifact manifest: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// readYAML accepts exactly one document with known keys.
func readYAML(filename string, value any) error {
	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open %s: %w", filename, err)
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("decode %s: %w", filename, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s: multiple YAML documents are not supported", filename)
		}
		return fmt.Errorf("decode %s: %w", filename, err)
	}
	return nil
}

// resolvePath interprets a relative path beside the main configuration.
func resolvePath(base, value string) string {
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(base, value)
}

// ValidName leaves room for temporary filename prefixes and suffixes.
func ValidName(value string) bool { return len(value) <= 240 && safeName.MatchString(value) }

// ValidRelativePath rejects absolute paths, traversal and glob characters.
func ValidRelativePath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\?#\x00\r\n") {
		return false
	}
	for _, p := range strings.Split(value, "/") {
		if !ValidName(p) {
			return false
		}
	}
	return true
}

// Within reports whether child is contained in, or equal to, parent.
func Within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ParseTimezone supports IANA names and fixed UTC offsets independently of the host.
func ParseTimezone(value string) (*time.Location, error) {
	if p := fixedZone.FindStringSubmatch(value); p != nil {
		h, _ := strconv.Atoi(p[2])
		m, _ := strconv.Atoi(p[3])
		if h > 23 || m > 59 {
			return nil, fmt.Errorf("invalid UTC offset %q", value)
		}
		offset := h*3600 + m*60
		if p[1] == "-" {
			offset = -offset
		}
		return time.FixedZone(value, offset), nil
	}
	loc, err := time.LoadLocation(value)
	if err != nil || value == "" || value == "Local" {
		return nil, fmt.Errorf("invalid timezone %q", value)
	}
	return loc, nil
}

// Date resolves an optional date in the configured timezone.
func (c *Config) Date(value string, now time.Time) (string, error) {
	if value == "" {
		value = now.In(c.Location).Format(time.DateOnly)
	}
	if !ValidDate(value) {
		return "", fmt.Errorf("invalid date %q: expected YYYY-MM-DD", value)
	}
	return value, nil
}

// ValidDate requires a real, canonical date with a nonzero year.
func ValidDate(value string) bool {
	t, err := time.Parse(time.DateOnly, value)
	return err == nil && t.Year() > 0 && t.Format(time.DateOnly) == value
}

// SelectTargets applies global and workflow-specific enable switches.
func (c *Config) SelectTargets(requested []string, workflow string) ([]string, error) {
	enabled := func(t Target) bool {
		return t.Enabled && ((workflow == "repo" && t.Repo.Enabled) || (workflow == "dailybuild" && t.Dailybuild.Enabled))
	}
	if len(requested) == 0 {
		for name, t := range c.Targets {
			if enabled(t) {
				requested = append(requested, name)
			}
		}
	}
	if len(requested) == 0 {
		return nil, fmt.Errorf("no enabled %s targets", workflow)
	}
	selected := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, name := range requested {
		t, ok := c.Targets[name]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", name)
		}
		if !enabled(t) {
			return nil, fmt.Errorf("target %s is disabled for %s", name, workflow)
		}
		if seen[name] {
			return nil, fmt.Errorf("target %s was specified more than once", name)
		}
		seen[name] = true
		selected = append(selected, name)
	}
	sort.Strings(selected)
	return selected, nil
}

// RenderFilename expands placeholders and protects generated metadata filenames.
func (a Artifact) RenderFilename(date, target string) (string, error) {
	name := strings.NewReplacer("{date}", date, "{target}", target).Replace(a.Filename)
	if !ValidName(name) || name == "SHA256SUMS" || name == "index.html" {
		return "", fmt.Errorf("unsafe or reserved filename %q", name)
	}
	return name, nil
}

// HTTPURL requires an absolute HTTP(S) URL without credentials or a fragment.
func HTTPURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("expected an absolute HTTP(S) URL without credentials or a fragment")
	}
	return u, nil
}

// JoinURL treats openruyi:/ as a literal URL path, never as a scheme.
func JoinURL(base, relative string) (string, error) {
	u, err := HTTPURL(base)
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(u.Path, "/") || u.RawQuery != "" {
		return "", errors.New("base URL must end in / and have no query string")
	}
	if relative == "" || strings.ContainsAny(relative, "\\?#\x00\r\n") || strings.HasPrefix(relative, "/") {
		return "", errors.New("invalid relative source path")
	}
	for _, p := range strings.Split(relative, "/") {
		if p == ".." || p == "." {
			return "", errors.New("source path contains traversal")
		}
	}
	return u.ResolveReference(&url.URL{Path: relative}).String(), nil
}

// SourceURL resolves a static source; dynamic sources are discovered at execution.
func (c *Config) SourceURL(s ArtifactSource) (string, error) {
	if s.Type != "static" {
		return "", fmt.Errorf("source type %q is not static", s.Type)
	}
	if (s.URL == "") == (s.Path == "") {
		return "", errors.New("static sources require exactly one of url or path")
	}
	if s.URL != "" {
		u, err := HTTPURL(s.URL)
		if err != nil {
			return "", err
		}
		return u.String(), nil
	}
	return JoinURL(c.Sources.Dailybuild.BuildBaseURL, s.Path)
}

// validate checks schema, paths and per-workflow switches before any operation.
func (c *Config) validate() error {
	if c.Version != 1 || c.Manifest.Version != 1 {
		return errors.New("configuration and manifest version must both be 1")
	}
	var err error
	c.Location, err = ParseTimezone(c.Timezone)
	if err != nil {
		return err
	}
	for _, name := range []string{c.Layout.Repo.Rolling, c.Layout.Releases.Rolling, c.Layout.Archive.RepoRoot, c.Layout.Archive.Repo.Rolling, c.Dailybuild.CurrentLink} {
		if !ValidName(name) {
			return fmt.Errorf("unsafe layout or link name %q", name)
		}
	}
	if ValidDate(c.Dailybuild.CurrentLink) || strings.TrimSpace(c.Dailybuild.Channel) == "" {
		return errors.New("invalid dailybuild current_link or channel")
	}
	roots := []string{c.Paths.RepoRoot, c.Paths.ReleasesRoot, c.Paths.ArchiveRoot}
	for i, first := range roots {
		if first == "/" {
			return errors.New("managed root must not be /")
		}
		for _, second := range roots[i+1:] {
			if Within(first, second) || Within(second, first) {
				return errors.New("repo, release and archive roots must not overlap")
			}
		}
		if Within(c.Paths.StagingRoot, first) {
			return errors.New("staging_root must not contain a managed root")
		}
	}
	if Within(filepath.Join(c.Paths.ReleasesRoot, c.Layout.Releases.Rolling), c.Paths.StagingRoot) || Within(c.Paths.RepoRoot, c.Paths.StagingRoot) || Within(c.Paths.ArchiveRoot, c.Paths.StagingRoot) {
		return errors.New("staging_root must be outside published content")
	}
	if c.Retention.DailybuildDays < 1 || c.Retention.DailybuildDays > 36500 {
		return errors.New("retention.dailybuild_days must be between 1 and 36500")
	}
	if len(c.Targets) == 0 || len(c.Manifest.Artifacts) == 0 {
		return errors.New("targets and artifact manifest must not be empty")
	}
	for name, t := range c.Targets {
		if name != "rva20" && name != "rva23" && name != "x86_64" {
			return fmt.Errorf("unsupported target %q: expected rva20, rva23 or x86_64", name)
		}
		if t.Dailybuild.Enabled && name == "x86_64" {
			return errors.New("x86_64 dailybuild is not supported")
		}
		if t.Repo.Enabled {
			if !ValidRelativePath(t.Repo.SourcePath) {
				return fmt.Errorf("target %s has an invalid repo source_path", name)
			}
			if _, err := JoinURL(c.Sources.Repo.RollingBaseURL, t.Repo.SourcePath); err != nil {
				return fmt.Errorf("target %s repo URL: %w", name, err)
			}
		}
	}
	return c.validateManifest()
}

// validateManifest rejects ambiguous names and incomplete source definitions.
func (c *Config) validateManifest() error {
	ids := map[string]bool{}
	names := map[string]map[string]bool{}
	for _, a := range c.Manifest.Artifacts {
		if !ValidName(a.ID) || ids[a.ID] {
			return fmt.Errorf("invalid or duplicate artifact id %q", a.ID)
		}
		ids[a.ID] = true
		if a.Required == nil || strings.TrimSpace(a.Description) == "" || len(a.Sources) == 0 {
			return fmt.Errorf("artifact %s requires description, required and sources", a.ID)
		}
		for name, s := range a.Sources {
			t, ok := c.Targets[name]
			if !ok {
				return fmt.Errorf("artifact %s references unknown target %s", a.ID, name)
			}
			filename, err := a.RenderFilename("2000-01-01", name)
			if err != nil {
				return err
			}
			if names[name] == nil {
				names[name] = map[string]bool{}
			}
			if names[name][filename] {
				return fmt.Errorf("duplicate filename %s for target %s", filename, name)
			}
			names[name][filename] = true
			if err := c.validateSource(t, s); err != nil {
				return fmt.Errorf("artifact %s, target %s: %w", a.ID, name, err)
			}
		}
	}
	for name, t := range c.Targets {
		if t.Enabled && t.Dailybuild.Enabled && len(names[name]) == 0 {
			return fmt.Errorf("enabled dailybuild target %s has no artifacts", name)
		}
	}
	return nil
}

// validateSource rejects fields for other source types as well as unsafe extraction paths.
func (c *Config) validateSource(t Target, s ArtifactSource) error {
	switch s.Type {
	case "static":
		if s.Match != "" || s.Package != "" || s.Architecture != "" || s.InternalPath != "" {
			return errors.New("static source contains extraction fields")
		}
		_, err := c.SourceURL(s)
		return err
	case "github":
		g := c.Sources.Dailybuild.GitHub
		if !ValidName(g.Owner) || !ValidName(g.Repository) || !ValidName(g.Workflow) || !ValidName(g.ArtifactName) || !envName.MatchString(g.TokenEnv) {
			return errors.New("complete GitHub owner, repository, workflow, artifact_name and token_env are required")
		}
		if s.Match == "" || strings.HasPrefix(s.Match, "/") || strings.Contains(s.Match, "..") || strings.ContainsAny(s.Match, "\\\x00\r\n") {
			return errors.New("unsafe ZIP match pattern")
		}
		if _, err := path.Match(s.Match, "example"); err != nil {
			return err
		}
		if s.URL != "" || s.Path != "" || s.Package != "" || s.Architecture != "" || s.InternalPath != "" {
			return errors.New("GitHub source contains fields for another source type")
		}
	case "rpm":
		if !ValidName(s.Package) || !ValidName(s.Architecture) || !ValidRelativePath(s.InternalPath) {
			return errors.New("RPM source needs a valid package, architecture and internal_path")
		}
		if _, err := JoinURL(t.Dailybuild.OVMFBaseURL, "repodata/repomd.xml"); err != nil {
			return err
		}
		if s.URL != "" || s.Path != "" || s.Match != "" {
			return errors.New("RPM source contains fields for another source type")
		}
	default:
		return fmt.Errorf("unsupported source type %q", s.Type)
	}
	return nil
}
