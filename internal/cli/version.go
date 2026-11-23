package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Version can be overridden at build time through -ldflags -X.
var Version = "dev"

// GitCommit and BuildDate are optional linker-injected build metadata.
var GitCommit, BuildDate string

// versionText is shared by version and --version.
func versionText() string {
	lines := []string{"Application Version: " + Version}
	if GitCommit != "" {
		lines = append(lines, "Git Commit: "+GitCommit)
	}
	if BuildDate != "" {
		lines = append(lines, "Build Date: "+BuildDate)
	}
	return strings.Join(lines, "\n")
}

// newVersionCommand prints the same version as the root --version flag.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the ruyi-deck version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionText())
			return err
		},
	}
}
