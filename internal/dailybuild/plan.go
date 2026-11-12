// Package dailybuild prepares and independently publishes dailybuild targets.
package dailybuild

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/apperror"
	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// Options contains the selected targets, date and preview mode.
type Options struct {
	Targets []string
	Date    string
	DryRun  bool
}

// artifactPlan connects a manifest entry to its final name and source.
type artifactPlan struct {
	ID          string
	Filename    string
	Description string
	Required    bool
	Source      config.ArtifactSource
	URL         string
}

// targetPlan contains all artifacts applicable to one target.
type targetPlan struct {
	Name      string
	Artifacts []artifactPlan
}

// plan describes operations without performing network requests or writes.
type plan struct {
	Date           string
	Root           string
	Target         string
	Targets        []targetPlan
	Current        string
	UpdateCurrent  bool
	Expired        []string
	MissingTargets []string
}

// targetArtifacts resolves static URLs while leaving dynamic sources for execution.
func targetArtifacts(cfg *config.Config, date, name string) ([]artifactPlan, error) {
	var artifacts []artifactPlan
	for _, entry := range cfg.Manifest.Artifacts {
		source, ok := entry.Sources[name]
		if !ok {
			continue
		}
		filename, err := entry.RenderFilename(date, name)
		if err != nil {
			return nil, err
		}
		artifact := artifactPlan{ID: entry.ID, Filename: filename, Description: entry.Description, Required: *entry.Required, Source: source}
		if source.Type == "static" {
			artifact.URL, err = cfg.SourceURL(source)
			if err != nil {
				return nil, err
			}
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

// prepare inspects all destinations and dependencies before any modification.
func prepare(cfg *config.Config, options Options, now time.Time) (*plan, error) {
	date, err := cfg.Date(options.Date, now)
	if err != nil {
		return nil, apperror.Input(err)
	}
	selected, err := cfg.SelectTargets(options.Targets, "dailybuild")
	if err != nil {
		return nil, apperror.Input(err)
	}
	p := &plan{Date: date, Root: filepath.Join(cfg.Paths.ReleasesRoot, cfg.Layout.Releases.Rolling)}
	p.Target = filepath.Join(p.Root, date)
	for _, name := range selected {
		artifacts, err := targetArtifacts(cfg, date, name)
		if err != nil {
			return nil, apperror.Input(err)
		}
		p.Targets = append(p.Targets, targetPlan{Name: name, Artifacts: artifacts})
	}
	if err := fsutil.CheckPrograms(requiredPrograms(p)); err != nil {
		return nil, err
	}
	if err := fsutil.CheckDirectoryPath(p.Target); err != nil {
		return nil, err
	}
	if err := fsutil.SameFilesystem(cfg.Paths.StagingRoot, p.Root); err != nil {
		return nil, err
	}
	if err := inspectDate(cfg, p); err != nil {
		return nil, err
	}
	for _, name := range selected {
		destination := filepath.Join(p.Target, name)
		if _, err := os.Lstat(destination); err == nil {
			return nil, fmt.Errorf("target %s already exists for %s; refusing to overwrite %s", name, date, destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	p.Current, err = readCurrent(p.Root, cfg.Dailybuild.CurrentLink)
	if err != nil {
		return nil, err
	}
	p.UpdateCurrent = p.Current == "" || date > p.Current
	protected := p.Current
	if p.UpdateCurrent {
		protected = date
	}
	p.Expired, err = expiredDirectories(p.Root, now, cfg, protected, date)
	if err != nil {
		return nil, err
	}
	p.MissingTargets, err = missingTargets(cfg, p, selected)
	return p, err
}

// inspectDate rejects unexpected entries and incomplete previously published targets.
func inspectDate(cfg *config.Config, p *plan) error {
	entries, err := os.ReadDir(p.Target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := cfg.Targets[name]; !ok || (name != "rva20" && name != "rva23") {
			return fmt.Errorf("unexpected entry in date directory: %s", filepath.Join(p.Target, name))
		}
		if _, err := filesOnDisk(cfg, p, name); err != nil {
			return err
		}
	}
	return nil
}

// filesOnDisk uses the manifest to validate and describe a published target.
func filesOnDisk(cfg *config.Config, p *plan, name string) ([]artifactFile, error) {
	directory := filepath.Join(p.Target, name)
	if err := fsutil.CheckDirectoryPath(directory); err != nil {
		return nil, err
	}
	artifacts, err := targetArtifacts(cfg, p.Date, name)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{"SHA256SUMS": true, "index.html": true}
	var files []artifactFile
	for _, artifact := range artifacts {
		allowed[artifact.Filename] = true
		file := artifactFile{artifactPlan: artifact}
		filename := filepath.Join(directory, artifact.Filename)
		if _, err := os.Lstat(filename); errors.Is(err, os.ErrNotExist) && !artifact.Required {
			files = append(files, file)
			continue
		}
		if err := checkFile(filename); err != nil {
			return nil, err
		}
		file.Present = true
		files = append(files, file)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return nil, fmt.Errorf("unexpected published file: %s", filepath.Join(directory, entry.Name()))
		}
		if err := checkFile(filepath.Join(directory, entry.Name())); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"SHA256SUMS", "index.html"} {
		if err := checkFile(filepath.Join(directory, name)); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// missingTargets includes enabled dailybuild targets only; planned names may be assumed present.
func missingTargets(cfg *config.Config, p *plan, planned []string) ([]string, error) {
	var missing []string
	for name, target := range cfg.Targets {
		if !target.Enabled || !target.Dailybuild.Enabled || contains(planned, name) {
			continue
		}
		info, err := os.Lstat(filepath.Join(p.Target, name))
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, name)
		} else if err != nil {
			return nil, err
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("target %s must be a real directory", name)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// validDate shares canonical date validation with configuration and retention.
func validDate(value string) bool { return config.ValidDate(value) }

// requiredPrograms checks RPM tools even when the selected firmware is optional.
func requiredPrograms(p *plan) []string {
	for _, target := range p.Targets {
		for _, artifact := range target.Artifacts {
			if artifact.Source.Type == "rpm" {
				return []string{"rpm2cpio", "cpio"}
			}
		}
	}
	return nil
}

// contains reports whether a target list contains a name.
func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// printMissing emits the same missing-target notice used in each HTML index.
func printMissing(out io.Writer, date string, missing []string) {
	if len(missing) > 0 {
		fmt.Fprintf(out, "Warning: Dailybuild %s is incomplete.\nMissing targets: %s\n", date, strings.Join(missing, ", "))
	}
}

// printPlan shows dynamic discovery without making network requests during dry-run.
func printPlan(out io.Writer, cfg *config.Config, p *plan) {
	fmt.Fprintf(out, "Release directory: %s\nStaging root: %s\nTimezone: %s\n", p.Target, cfg.Paths.StagingRoot, cfg.Timezone)
	for _, target := range p.Targets {
		fmt.Fprintf(out, "Target: %s\n", target.Name)
		for _, artifact := range target.Artifacts {
			switch artifact.Source.Type {
			case "static":
				fmt.Fprintf(out, "  Download %s\n", artifact.URL)
			case "github":
				g := cfg.Sources.Dailybuild.GitHub
				fmt.Fprintf(out, "  Discover latest successful run: https://api.github.com/repos/%s/%s/actions/workflows/%s/runs?status=success\n  Artifact: %s; ZIP match: %s; token environment: %s\n", g.Owner, g.Repository, g.Workflow, g.ArtifactName, artifact.Source.Match, g.TokenEnv)
			case "rpm":
				fmt.Fprintf(out, "  Discover %s (%s) using %srepodata/repomd.xml or directory index\n  Extract %s with rpm2cpio/cpio\n", artifact.Source.Package, artifact.Source.Architecture, cfg.Targets[target.Name].Dailybuild.OVMFBaseURL, artifact.Source.InternalPath)
			}
			fmt.Fprintf(out, "    -> %s (required: %t)\n", filepath.Join(p.Target, target.Name, artifact.Filename), artifact.Required)
		}
		fmt.Fprintf(out, "  Generate SHA256SUMS and index.html in %s\n", filepath.Join(p.Target, target.Name))
	}
	if p.UpdateCurrent {
		fmt.Fprintf(out, "Update %s -> %s after at least one successful target\n", filepath.Join(p.Root, cfg.Dailybuild.CurrentLink), p.Date)
	} else {
		fmt.Fprintf(out, "Keep %s -> %s\n", filepath.Join(p.Root, cfg.Dailybuild.CurrentLink), p.Current)
	}
	fmt.Fprintln(out, "Refresh missing-target notices in this date's existing index.html files after publication.")
	printMissing(out, p.Date, p.MissingTargets)
	for _, directory := range p.Expired {
		fmt.Fprintf(out, "Remove expired dailybuild after successful publication: %s\n", directory)
	}
}
