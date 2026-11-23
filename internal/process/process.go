// Package process runs external programs without invoking a shell.
package process

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// RunFunc permits unit tests to supply a fake external program runner.
type RunFunc func(context.Context, io.Writer, string, ...string) error

// Run streams output and checks the program's exit status, including cancellation.
func Run(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
