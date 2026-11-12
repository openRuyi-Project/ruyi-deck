package dailybuild

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/openRuyi-Project/ruyi-deck/internal/fsutil"
)

// artifactFile tracks both downloaded artifacts and optional missing entries.
type artifactFile struct {
	artifactPlan
	Present bool
}

// writeChecksums hashes actual manifest files in stable filename order.
func writeChecksums(ctx context.Context, directory string, files []artifactFile) error {
	ordered := append([]artifactFile(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Filename < ordered[j].Filename })
	out, err := os.OpenFile(filepath.Join(directory, "SHA256SUMS"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("create SHA256SUMS: %w", err)
	}
	defer out.Close()
	writer := bufio.NewWriter(out)
	for _, file := range ordered {
		if !file.Present {
			continue
		}
		sum, err := checksum(ctx, filepath.Join(directory, file.Filename))
		if err != nil {
			return fmt.Errorf("checksum %s: %w", file.Filename, err)
		}
		if _, err := fmt.Fprintf(writer, "%x  %s\n", sum, file.Filename); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("write SHA256SUMS: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close SHA256SUMS: %w", err)
	}
	return nil
}

// checksum computes SHA256 without loading a release image into memory.
func checksum(ctx context.Context, filename string) ([]byte, error) {
	in, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	hash := sha256.New()
	if _, err := fsutil.Copy(ctx, hash, in); err != nil {
		return nil, err
	}
	return hash.Sum(nil), nil
}

// checkFile requires a nonempty regular file, never a symlink or directory.
func checkFile(filename string) error {
	info, err := os.Lstat(filename)
	if err != nil {
		return fmt.Errorf("inspect release file %s: %w", filename, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("release file must be a nonempty regular file: %s", filename)
	}
	return nil
}

// verifyFiles checks the complete target before it becomes a public release.
func verifyFiles(directory string, files []artifactFile) error {
	for _, file := range files {
		if file.Required && !file.Present {
			return fmt.Errorf("required artifact is missing: %s", file.Filename)
		}
		if file.Present {
			if err := checkFile(filepath.Join(directory, file.Filename)); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"SHA256SUMS", "index.html"} {
		if err := checkFile(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}
