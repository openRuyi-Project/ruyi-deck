// Package repo synchronizes and archives rolling repositories using DNF 4 reposync.
package repo

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/apperror"
	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
	"github.com/openRuyi-Project/ruyi-deck/internal/process"
)

// Options supplies shared flags for rolling sync and archive.
type Options struct {
	Targets []string
	Date    string
	DryRun  bool
}

// operation identifies the source, destination and incremental behavior of a target.
type operation struct {
	Target      string
	Source      string
	Destination string
	Incremental bool
}

// plan groups the fully validated operations for one invocation.
type plan struct {
	Root       string
	RepoRoot   string
	Archive    bool
	Operations []operation
}

// Sync synchronizes selected targets, stopping at the first failure.
func Sync(ctx context.Context, cfg *config.Config, options Options, out io.Writer) error {
	return execute(ctx, cfg, options, false, out, process.Run)
}

// Archive snapshots valid local repositories without deleting or replacing archives.
func Archive(ctx context.Context, cfg *config.Config, options Options, out io.Writer) error {
	return execute(ctx, cfg, options, true, out, process.Run)
}

// validMetadata requires a readable, nonempty repomd.xml under real directories.
func validMetadata(directory string) error {
	metadataDir := filepath.Join(directory, "repodata")
	if err := fsutil.CheckDirectoryPath(metadataDir); err != nil {
		return err
	}
	filename := filepath.Join(metadataDir, "repomd.xml")
	info, err := os.Lstat(filename)
	if err != nil {
		return fmt.Errorf("repository metadata %s: %w", filename, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("invalid repository metadata: %s", filename)
	}
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := xml.NewDecoder(io.LimitReader(f, 16<<20))
	for {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("invalid repomd.xml %s: %w", filename, err)
		}
		if start, ok := token.(xml.StartElement); ok {
			if start.Name.Local != "repomd" {
				return fmt.Errorf("invalid repomd.xml root in %s", filename)
			}
			return nil
		}
	}
}

// repoState distinguishes an absent/empty repository from a valid existing one.
func repoState(directory string) (bool, error) {
	if err := fsutil.CheckDirectoryPath(directory); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return false, nil
	}
	if err := validMetadata(directory); err != nil {
		return false, fmt.Errorf("nonempty directory is not a valid repository: %s: %w", directory, err)
	}
	return true, nil
}

// prepare inspects all targets before commands or directory creation begin.
func prepare(cfg *config.Config, options Options, archive bool, now time.Time) (*plan, error) {
	names, err := cfg.SelectTargets(options.Targets, "repo")
	if err != nil {
		return nil, apperror.Input(err)
	}
	p := &plan{RepoRoot: filepath.Join(cfg.Paths.RepoRoot, cfg.Layout.Repo.Rolling), Archive: archive}
	p.Root = p.RepoRoot
	if archive {
		date, err := cfg.Date(options.Date, now)
		if err != nil {
			return nil, apperror.Input(err)
		}
		p.Root = filepath.Join(cfg.Paths.ArchiveRoot, cfg.Layout.Archive.RepoRoot, cfg.Layout.Archive.Repo.Rolling, date[:4], date[5:7], date[8:10])
	}
	for _, directory := range []string{p.RepoRoot, p.Root} {
		if err := fsutil.CheckDirectoryPath(directory); err != nil {
			return nil, err
		}
	}
	for _, name := range names {
		op := operation{Target: name, Destination: filepath.Join(p.Root, name)}
		if archive {
			source := filepath.Join(p.RepoRoot, name)
			if err := validMetadata(source); err != nil {
				return nil, fmt.Errorf("archive target %s source %s: %w", name, source, err)
			}
			op.Source = (&url.URL{Scheme: "file", Path: source}).String()
			if _, err := os.Lstat(op.Destination); err == nil {
				return nil, fmt.Errorf("archive target %s already exists: %s", name, op.Destination)
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		} else {
			op.Source, err = config.JoinURL(cfg.Sources.Repo.RollingBaseURL, cfg.Targets[name].Repo.SourcePath)
			if err != nil {
				return nil, apperror.Input(err)
			}
			op.Incremental, err = repoState(op.Destination)
			if err != nil {
				return nil, fmt.Errorf("sync target %s: %w", name, err)
			}
		}
		p.Operations = append(p.Operations, op)
	}
	return p, nil
}

// arguments isolates one configured repository from system-enabled repositories.
func arguments(op operation, parent string) []string {
	// DNF 4 --repoid already selects only this repository. It must not be
	// combined with --disablerepo, which conflicts with that selection mode.
	args := []string{"--setopt=logfilelevel=0", "--repofrompath=" + op.Target + "," + op.Source,
		"--setopt=" + op.Target + ".skip_if_unavailable=False", "reposync", "--repoid=" + op.Target, "--download-path=" + parent, "--download-metadata"}
	if op.Incremental {
		args = append(args, "--delete")
	}
	return args
}

// printPlan makes deletion behavior and each independent DNF argument visible.
func printPlan(out io.Writer, p *plan) {
	for _, op := range p.Operations {
		fmt.Fprintf(out, "Target: %s\nSource: %s\nDestination: %s\n", op.Target, op.Source, op.Destination)
		parent := p.Root
		if !op.Incremental {
			parent = filepath.Join(p.Root, ".reposync-<temporary>")
		}
		fmt.Fprintf(out, "DNF plan (temporary name is allocated during execution): dnf %q\n", arguments(op, parent))
		if op.Incremental {
			fmt.Fprintln(out, "Warning: incremental sync uses --delete. A failure can leave partial updates or deletions; no rollback is provided.")
		}
	}
}

// execute checks dependencies before writes; dry-run never starts DNF.
func execute(ctx context.Context, cfg *config.Config, options Options, archive bool, out io.Writer, run process.RunFunc) (err error) {
	now := time.Now()
	p, err := prepare(cfg, options, archive, now)
	if err != nil {
		return err
	}
	if err := fsutil.CheckPrograms([]string{"dnf"}); err != nil {
		return err
	}
	if options.DryRun {
		fmt.Fprintln(out, "Dry-run: no DNF commands or filesystem changes will be performed. The reposync plugin is checked on real execution.")
		printPlan(out, p)
		return nil
	}
	if err := run(ctx, io.Discard, "dnf", "--setopt=logfilelevel=0", "reposync", "--help"); err != nil {
		return fmt.Errorf("dnf reposync is unavailable (install the DNF 4 reposync plugin): %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fsutil.EnsureDirectory(p.RepoRoot, 0755); err != nil {
		return err
	}
	lock, err := fsutil.AcquireLock(p.RepoRoot)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if archive {
		// Lock ordering is always repository first, archive second.
		archiveRoot := filepath.Join(cfg.Paths.ArchiveRoot, cfg.Layout.Archive.RepoRoot, cfg.Layout.Archive.Repo.Rolling)
		if err := fsutil.EnsureDirectory(archiveRoot, 0755); err != nil {
			return err
		}
		archiveLock, err := fsutil.AcquireLock(archiveRoot)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, archiveLock.Close()) }()
	}
	p, err = prepare(cfg, options, archive, now)
	if err != nil {
		return err
	}
	printPlan(out, p)
	var completed []string
	for _, op := range p.Operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := perform(ctx, out, op, p, run); err != nil {
			fmt.Fprintf(out, "Successful targets: %s\nFailed target: %s\n", strings.Join(completed, ", "), op.Target)
			return fmt.Errorf("repo target %s, source %s, destination %s: %w", op.Target, op.Source, op.Destination, err)
		}
		completed = append(completed, op.Target)
		fmt.Fprintf(out, "Completed target: %s\n", op.Target)
	}
	fmt.Fprintf(out, "Successful targets: %s\n", strings.Join(completed, ", "))
	return nil
}

// perform stages first syncs and archives; only incremental sync modifies a live repo.
func perform(ctx context.Context, out io.Writer, op operation, p *plan, run process.RunFunc) (err error) {
	if op.Incremental {
		if err := run(ctx, out, "dnf", arguments(op, p.Root)...); err != nil {
			return err
		}
		return validMetadata(op.Destination)
	}
	if err := fsutil.EnsureDirectory(p.Root, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(p.Root, ".reposync-*")
	if err != nil {
		return err
	}
	defer func() {
		if e := fsutil.RemoveChildDirectory(p.Root, stage); e != nil {
			err = errors.Join(err, e)
		}
	}()
	if err := run(ctx, out, "dnf", arguments(op, stage)...); err != nil {
		return err
	}
	stagedTarget := filepath.Join(stage, op.Target)
	if err := validMetadata(stagedTarget); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.Archive {
		// os.Remove succeeds only for an empty pre-existing directory.
		if _, err := os.Lstat(op.Destination); err == nil {
			if err := os.Remove(op.Destination); err != nil {
				return fmt.Errorf("first-sync destination is no longer empty: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return fsutil.RenameNewDirectory(stagedTarget, op.Destination)
}
