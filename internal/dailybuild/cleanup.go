package dailybuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// expiredDirectories lists real date directories older than the retention window.
// The active current and the release being created are protected for this run.
func expiredDirectories(root string, now time.Time, cfg *config.Config, current, release string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list dailybuild directory: %w", err)
	}
	local := now.In(cfg.Location)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, cfg.Location)
	cutoff := today.AddDate(0, 0, 1-cfg.Retention.DailybuildDays).Format(time.DateOnly)
	var expired []string
	for _, entry := range entries {
		name := entry.Name()
		if !validDate(name) || name == current || name == release || name >= cutoff {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		expired = append(expired, filepath.Join(root, name))
	}
	sort.Strings(expired)
	return expired, nil
}

// cleanupExpired recomputes the plan after a successful current update.
func cleanupExpired(ctx context.Context, out io.Writer, cfg *config.Config, p *plan, now time.Time) error {
	current, err := readCurrent(p.Root, cfg.Dailybuild.CurrentLink)
	if err != nil {
		return err
	}
	expired, err := expiredDirectories(p.Root, now, cfg, current, p.Date)
	if err != nil {
		return err
	}
	for _, directory := range expired {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fsutil.RemoveChildDirectory(p.Root, directory); err != nil {
			return fmt.Errorf("remove expired dailybuild %s: %w", directory, err)
		}
		fmt.Fprintf(out, "Removed expired dailybuild: %s\n", directory)
	}
	return nil
}
