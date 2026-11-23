// Package cli defines the Cobra command tree and maps errors to process exit codes.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/openRuyi-Project/ruyi-deck/internal/apperror"
	"github.com/spf13/cobra"
)

// Execute runs the command tree and prints each error exactly once.
func Execute(ctx context.Context) int {
	root := newRootCommand()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "Error: interrupted: %s\n", err)
		return 130
	}
	// Cobra creates unknown-command errors before a command's Args hook runs.
	if strings.HasPrefix(err.Error(), "unknown command ") {
		err = apperror.Input(err)
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", err)
	return apperror.ExitCode(err)
}

// newRootCommand builds commands without reading configuration for help or version.
func newRootCommand() *cobra.Command {
	var configFile string
	root := &cobra.Command{
		Use:           "ruyi-deck",
		Short:         "Manage openRuyi repositories and releases",
		Version:       versionText(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		RunE:          missingSubcommand,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return apperror.Input(err) })
	root.PersistentFlags().StringVar(&configFile, "config", defaultConfigPath(), "Path to YAML configuration (default: config/config.yaml beside the executable)")
	root.AddCommand(newVersionCommand(), newReleasesCommand(&configFile), newRepoCommand(&configFile))
	return root
}

// defaultConfigPath follows the deployed executable, independently of the working directory.
func defaultConfigPath() string {
	executable, err := os.Executable()
	if err != nil {
		return "config/config.yaml"
	}
	return filepath.Join(filepath.Dir(executable), "config", "config.yaml")
}

// noArgs rejects positional arguments on commands that use only named flags.
func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return apperror.Input(err)
	}
	return nil
}

// missingSubcommand makes incomplete invocations fail with a helpful input error.
func missingSubcommand(cmd *cobra.Command, _ []string) error {
	return apperror.Invalid("a subcommand is required; run %s --help", cmd.CommandPath())
}
