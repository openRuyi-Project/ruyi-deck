//go:build linux

// Package fsutil contains the Linux filesystem checks used before publishing.
package fsutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// CheckDirectoryPath rejects symlinks and nondirectories in every existing component.
// Missing suffix components are allowed so the same check works during dry-run.
func CheckDirectoryPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("expected an absolute directory path: %s", path)
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect directory %s: %w", current, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("directory component must be a real directory, not a symlink: %s", current)
		}
	}
	return nil
}

// EnsureDirectory creates a checked directory tree with the process umask applied.
func EnsureDirectory(path string, mode os.FileMode) error {
	if err := CheckDirectoryPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	return CheckDirectoryPath(path)
}

// existingDevice finds the filesystem containing a path or its nearest existing parent.
func existingDevice(path string) (uint64, error) {
	if err := CheckDirectoryPath(path); err != nil {
		return 0, err
	}
	for {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("inspect filesystem for %s: %w", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return 0, fmt.Errorf("cannot inspect Linux filesystem for %s", path)
		}
		return uint64(stat.Dev), nil
	}
}

// SameFilesystem prevents a cross-device rename at the end of a long download.
func SameFilesystem(first, second string) error {
	a, err := existingDevice(first)
	if err != nil {
		return err
	}
	b, err := existingDevice(second)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("staging and release directories must be on the same filesystem: %s, %s", first, second)
	}
	return nil
}

// CheckPrograms checks only the external programs declared by the selected workflow.
func CheckPrograms(names []string) error {
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("required external program %q is unavailable: %w", name, err)
		}
	}
	return nil
}

// Lock represents an advisory lock for one ruyi-deck management area.
type Lock struct {
	file *os.File
}

// AcquireLock prevents simultaneous ruyi-deck operations in the same area.
// The small lock file intentionally remains after release; flock is process scoped.
func AcquireLock(root string) (*Lock, error) {
	name := filepath.Join(root, ".ruyi-deck.lock")
	fd, err := syscall.Open(name, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, fmt.Errorf("open operation lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), name)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect operation lock: %w", err)
		}
		return nil, errors.New("operation lock must be a regular file")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another operation may be running; cannot acquire lock: %w", err)
	}
	return &Lock{file: f}, nil
}

// Close releases the advisory lock when its file descriptor is closed.
func (l *Lock) Close() error { return l.file.Close() }

// RenameNewDirectory publishes a directory after checking that its name is unused.
// Callers hold the operation lock; managed directories must not be edited concurrently.
func RenameNewDirectory(source, target string) error {
	if err := CheckDirectoryPath(filepath.Dir(target)); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("refusing to overwrite existing release: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("publish %s: %w", target, err)
	}
	return nil
}

// RemoveChildDirectory removes only a real, direct child of the checked parent.
// os.RemoveAll does not follow symbolic links encountered inside the tree.
func RemoveChildDirectory(parent, child string) error {
	if filepath.Dir(child) != parent || filepath.Base(child) == "." || child == parent {
		return fmt.Errorf("refusing removal outside parent %s: %s", parent, child)
	}
	if err := CheckDirectoryPath(parent); err != nil {
		return err
	}
	info, err := os.Lstat(child)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing recursive removal of a nondirectory or symlink: %s", child)
	}
	return os.RemoveAll(child)
}

// contextReader makes large downloads and checksum reads interruptible.
type contextReader struct {
	ctx context.Context
	in  io.Reader
}

// Read checks cancellation before each read from the underlying stream.
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(p)
}

// Copy streams data with a bounded buffer and checks cancellation between reads.
func Copy(ctx context.Context, out io.Writer, in io.Reader) (int64, error) {
	return io.Copy(out, contextReader{ctx: ctx, in: in})
}
