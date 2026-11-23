package dailybuild

import (
	"errors"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/templates"
)

// indexFile contains actual filesystem metadata for a single table row.
type indexFile struct {
	Name        string
	Modified    string
	Size        string
	Description string
	Present     bool
}

// writeIndex renders the embedded template with html/template escaping enabled.
func writeIndex(directory string, cfg *config.Config, date, target string, files []artifactFile, missing []string, now time.Time) (err error) {
	page, err := template.New("index").Parse(templates.Index)
	if err != nil {
		return fmt.Errorf("parse index template: %w", err)
	}
	rows := make([]indexFile, 0, len(files)+1)
	for _, file := range files {
		row := indexFile{Name: file.Filename, Description: file.Description, Present: file.Present}
		if file.Present {
			if err := row.readMetadata(directory, cfg.Location); err != nil {
				return err
			}
		}
		rows = append(rows, row)
	}
	sums := indexFile{Name: "SHA256SUMS", Description: "SHA256 checksums", Present: true}
	if err := sums.readMetadata(directory, cfg.Location); err != nil {
		return err
	}
	rows = append(rows, sums)
	variables := map[string]any{
		"release":         date,
		"target":          target,
		"target_upper":    strings.ToUpper(target),
		"channel":         cfg.Dailybuild.Channel,
		"copyright_year":  now.In(cfg.Location).Year(),
		"path":            "/" + cfg.Layout.Releases.Rolling + "/" + date + "/" + target,
		"files":           rows,
		"missing_targets": strings.Join(missing, ", "),
	}
	// Render beside the destination and rename so readers never see partial HTML.
	temporary, err := os.MkdirTemp(directory, ".index-*")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(temporary); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary index directory: %w", cleanupErr))
		}
	}()
	out, err := os.OpenFile(filepath.Join(temporary, "index.html"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create index.html: %w", err)
	}
	defer out.Close()
	if err := page.Execute(out, variables); err != nil {
		return fmt.Errorf("render index.html: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close index.html: %w", err)
	}
	return os.Rename(out.Name(), filepath.Join(directory, "index.html"))
}

// refreshIndexes updates generated notices after adding a same-date target.
func refreshIndexes(cfg *config.Config, p *plan, now time.Time) error {
	missing, err := missingTargets(cfg, p, nil)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(p.Target)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		files, err := filesOnDisk(cfg, p, entry.Name())
		if err != nil {
			return err
		}
		if err := writeIndex(filepath.Join(p.Target, entry.Name()), cfg, p.Date, entry.Name(), files, missing, now); err != nil {
			return err
		}
	}
	return nil
}

// readMetadata obtains sizes and modification times from the files being published.
func (row *indexFile) readMetadata(directory string, location *time.Location) error {
	info, err := os.Lstat(filepath.Join(directory, row.Name))
	if err != nil {
		return fmt.Errorf("index metadata for %s: %w", row.Name, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("index file is not a nonempty regular file: %s", row.Name)
	}
	row.Modified = info.ModTime().In(location).Format("2006-01-02 15:04:05 -0700")
	row.Size = formatSize(info.Size())
	return nil
}

// formatSize formats file sizes using binary units without losing small byte counts.
func formatSize(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	for _, unit := range units {
		value /= 1024
		if value < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return ""
}
