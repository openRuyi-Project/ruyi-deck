package cli

import (
	"github.com/openRuyi-Project/ruyi-deck/internal/apperror"
	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/repo"
	"github.com/spf13/cobra"
)

// newRepoCommand registers only the rolling workflows supported in version 1.
func newRepoCommand(configFile *string) *cobra.Command {
	root := &cobra.Command{Use: "repo", Short: "Manage local repositories", Args: noArgs, RunE: missingSubcommand}
	for _, archive := range []bool{false, true} {
		name, short := "sync", "Synchronize a rolling repository"
		if archive {
			name, short = "archive", "Archive local rolling repositories"
		}
		parent := &cobra.Command{Use: name, Short: short, Args: noArgs, RunE: missingSubcommand}
		parent.AddCommand(newRollingCommand(configFile, archive))
		root.AddCommand(parent)
	}
	return root
}

// newRollingCommand validates flags before dispatching to the repo package.
func newRollingCommand(configFile *string, archive bool) *cobra.Command {
	var options repo.Options
	command := &cobra.Command{Use: "rolling", Short: "Process enabled rolling repository targets", Args: noArgs}
	if !archive {
		command.Long = "Synchronize rolling repositories using DNF reposync. Existing valid repositories use --delete; failures can leave partial updates or deletions. Version 1 does not provide incremental rollback."
	}
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("target") && len(options.Targets) == 0 {
			return apperror.Invalid("--target must name at least one enabled target")
		}
		if archive && cmd.Flags().Changed("date") && options.Date == "" {
			return apperror.Invalid("--date must use YYYY-MM-DD")
		}
		cfg, err := config.Load(*configFile)
		if err != nil {
			return apperror.Input(err)
		}
		if archive {
			return repo.Archive(cmd.Context(), cfg, options, cmd.OutOrStdout())
		}
		return repo.Sync(cmd.Context(), cfg, options, cmd.OutOrStdout())
	}
	command.Flags().StringSliceVar(&options.Targets, "target", nil, "Comma-separated targets (default: all enabled repo targets)")
	command.Flags().BoolVar(&options.DryRun, "dry-run", false, "Show the plan without running DNF or changing files")
	if archive {
		command.Flags().StringVar(&options.Date, "date", "", "Archive date YYYY-MM-DD (default: today in the configured timezone)")
	}
	return command
}
