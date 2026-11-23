package dailybuild

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// readCurrent accepts only a relative date link to a real dailybuild directory.
func readCurrent(root, name string) (string, error) {
	link := filepath.Join(root, name)
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect current: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("current must be a symbolic link; refusing to replace %s", link)
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("read current link: %w", err)
	}
	date := strings.TrimSuffix(target, "/")
	if !validDate(date) {
		return "", fmt.Errorf("current must point to a relative YYYY-MM-DD directory: %s", link)
	}
	if err := fsutil.CheckDirectoryPath(filepath.Join(root, date)); err != nil {
		return "", err
	}
	info, err = os.Lstat(filepath.Join(root, date))
	if err != nil {
		return "", fmt.Errorf("current target is unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("current target must be a real directory")
	}
	return date, nil
}

// updateCurrent atomically replaces a link and preserves the old link on failure.
func updateCurrent(root, name, date string) (err error) {
	previous, err := readCurrent(root, name)
	if err != nil {
		return err
	}
	if previous >= date {
		return nil
	}
	if err := fsutil.CheckDirectoryPath(filepath.Join(root, date)); err != nil {
		return err
	}
	if info, err := os.Lstat(filepath.Join(root, date)); err != nil || !info.IsDir() {
		return fmt.Errorf("cannot update current: target dailybuild directory is unavailable")
	}
	temporary, err := os.MkdirTemp(root, ".current-*")
	if err != nil {
		return fmt.Errorf("create temporary current directory: %w", err)
	}
	defer func() {
		if cleanupErr := fsutil.RemoveChildDirectory(root, temporary); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary current directory: %w", cleanupErr))
		}
	}()
	link := filepath.Join(temporary, "link")
	if err := os.Symlink(date, link); err != nil {
		return fmt.Errorf("create current symbolic link: %w", err)
	}
	if err := os.Rename(link, filepath.Join(root, name)); err != nil {
		return fmt.Errorf("replace current symbolic link: %w", err)
	}
	return nil
}
