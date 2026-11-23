package cli

import (
	"github.com/openRuyi-Project/ruyi-deck/internal/apperror"
	"github.com/openRuyi-Project/ruyi-deck/internal/config"
	"github.com/openRuyi-Project/ruyi-deck/internal/dailybuild"
	"github.com/spf13/cobra"
)

// newReleasesCommand constructs releases create dailybuild and its flags.
func newReleasesCommand(configFile *string) *cobra.Command {
	releases := &cobra.Command{
		Use:   "releases",
		Short: "Manage release images",
		Args:  noArgs,
		RunE:  missingSubcommand,
	}
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a release",
		Args:  noArgs,
		RunE:  missingSubcommand,
	}
	var options dailybuild.Options
	daily := &cobra.Command{
		Use:   "dailybuild",
		Short: "Download and publish dailybuild artifacts for independent targets",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("target") && len(options.Targets) == 0 {
				return apperror.Invalid("--target must name at least one enabled target")
			}
			if cmd.Flags().Changed("date") && options.Date == "" {
				return apperror.Invalid("--date must be in YYYY-MM-DD format")
			}
			cfg, err := config.Load(*configFile)
			if err != nil {
				return apperror.Input(err)
			}
			return dailybuild.Create(cmd.Context(), cfg, options, cmd.OutOrStdout())
		},
	}
	daily.Flags().StringSliceVar(&options.Targets, "target", nil, "Comma-separated targets (default: all enabled dailybuild targets)")
	daily.Flags().StringVar(&options.Date, "date", "", "Release date in YYYY-MM-DD format (default: today in the configured timezone)")
	daily.Flags().BoolVar(&options.DryRun, "dry-run", false, "Validate and show the plan without downloads or filesystem changes")
	create.AddCommand(daily)
	releases.AddCommand(create)
	return releases
}
