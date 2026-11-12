package dailybuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// Create validates, previews or independently publishes the selected dailybuild targets.
func Create(ctx context.Context, cfg *config.Config, options Options, out io.Writer) (err error) {
	now := time.Now()
	p, err := prepare(cfg, options, now)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if options.DryRun {
		fmt.Fprintln(out, "Dry-run: no downloads, external commands or filesystem changes will be performed.")
		printPlan(out, cfg, p)
		return nil
	}
	if err := fsutil.EnsureDirectory(p.Root, 0755); err != nil {
		return err
	}
	lock, err := fsutil.AcquireLock(p.Root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	// Another invocation may have completed while this invocation was checking inputs.
	p, err = prepare(cfg, options, now)
	if err != nil {
		return err
	}
	printPlan(out, cfg, p)
	if err := fsutil.EnsureDirectory(cfg.Paths.StagingRoot, 0700); err != nil {
		return err
	}
	if err := fsutil.SameFilesystem(cfg.Paths.StagingRoot, p.Root); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(cfg.Paths.StagingRoot, ".dailybuild-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer func() {
		if cleanupErr := fsutil.RemoveChildDirectory(cfg.Paths.StagingRoot, staging); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean staging directory %s: %w", staging, cleanupErr))
		}
	}()
	client := newHTTPClient()
	defer client.CloseIdleConnections()
	store := &sourceStore{cfg: cfg, client: client, directory: staging, run: runExternal}
	var successful, failed []string
	var failures error
	for _, target := range p.Targets {
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		targetErr := createTarget(ctx, out, store, cfg, p, staging, target, now)
		if targetErr != nil {
			failed = append(failed, target.Name)
			failures = errors.Join(failures, fmt.Errorf("dailybuild target %s, destination %s: %w", target.Name, filepath.Join(p.Target, target.Name), targetErr))
			fmt.Fprintf(out, "Failed target: %s: %v\n", target.Name, targetErr)
			if ctx.Err() != nil {
				return errors.Join(failures, ctx.Err())
			}
			continue
		}
		successful = append(successful, target.Name)
		fmt.Fprintf(out, "Published target: %s\n", target.Name)
		// Refresh after each publication so a later failure cannot leave stale notices.
		if err := refreshIndexes(cfg, p, now); err != nil {
			return errors.Join(failures, fmt.Errorf("target was published, but index refresh failed: %w", err))
		}
	}
	fmt.Fprintf(out, "Successful targets: %s\nFailed targets: %s\n", strings.Join(successful, ", "), strings.Join(failed, ", "))
	if len(successful) == 0 {
		return failures
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(failures, err)
	}
	missing, err := missingTargets(cfg, p, nil)
	if err != nil {
		return errors.Join(failures, err)
	}
	printMissing(out, p.Date, missing)
	if err := updateCurrent(p.Root, cfg.Dailybuild.CurrentLink, p.Date); err != nil {
		return errors.Join(failures, fmt.Errorf("dailybuild was published, but current update failed: %w", err))
	}
	if err := cleanupExpired(ctx, out, cfg, p, now); err != nil {
		return errors.Join(failures, fmt.Errorf("dailybuild was published, but retention cleanup failed: %w", err))
	}
	return failures
}

// createTarget prepares one complete target and publishes it without touching other targets.
func createTarget(ctx context.Context, out io.Writer, store *sourceStore, cfg *config.Config, p *plan, staging string, target targetPlan, now time.Time) error {
	stagedDate := filepath.Join(staging, target.Name, p.Date)
	if err := os.MkdirAll(stagedDate, 0755); err != nil {
		return err
	}
	directory := filepath.Join(stagedDate, target.Name)
	if err := os.Mkdir(directory, 0755); err != nil {
		return err
	}
	var files []artifactFile
	present := 0
	for _, artifact := range target.Artifacts {
		if err := ctx.Err(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Preparing %s/%s\n", target.Name, artifact.Filename)
		file := artifactFile{artifactPlan: artifact}
		destination := filepath.Join(directory, artifact.Filename)
		err := store.fetch(ctx, target.Name, artifact, destination)
		if err == nil {
			err = checkFile(destination)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if artifact.Required {
				return fmt.Errorf("required artifact %s: %w", artifact.ID, err)
			}
			// An extraction or timestamp operation can fail after creating its output.
			if removeErr := os.Remove(destination); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			fmt.Fprintf(out, "Warning: optional artifact %s/%s unavailable: %v\n", target.Name, artifact.Filename, err)
			files = append(files, file)
			continue
		}
		file.Present = true
		present++
		files = append(files, file)
	}
	if present == 0 {
		return fmt.Errorf("refusing to publish a target without any artifacts")
	}
	if err := writeChecksums(ctx, directory, files); err != nil {
		return err
	}
	missing, err := missingTargets(cfg, p, []string{target.Name})
	if err != nil {
		return err
	}
	if err := writeIndex(directory, cfg, p.Date, target.Name, files, missing, now); err != nil {
		return err
	}
	if err := verifyFiles(directory, files); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishTarget(stagedDate, p, target.Name)
}

// publishTarget uses a directory rename to expose only complete content.
func publishTarget(stagedDate string, p *plan, name string) error {
	if _, err := os.Lstat(p.Target); errors.Is(err, os.ErrNotExist) {
		return fsutil.RenameNewDirectory(stagedDate, p.Target)
	} else if err != nil {
		return err
	}
	if err := fsutil.CheckDirectoryPath(p.Target); err != nil {
		return err
	}
	return fsutil.RenameNewDirectory(filepath.Join(stagedDate, name), filepath.Join(p.Target, name))
}
