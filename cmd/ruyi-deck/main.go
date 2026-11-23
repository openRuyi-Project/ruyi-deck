//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/openRuyi-Project/ruyi-deck/internal/cli"
)

// main cancels in-flight work on interruption and returns the CLI's exit code.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx)
	stop()
	os.Exit(code)
}
