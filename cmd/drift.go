package cmd

import (
	"context"

	"github.com/cloudquery/cloudquery/internal/analytics"
	"github.com/cloudquery/cloudquery/pkg/errors"
	"github.com/cloudquery/cloudquery/pkg/module/drift"
	"github.com/cloudquery/cloudquery/pkg/ui/console"
	"github.com/spf13/cobra"
)

var (
	driftCmd = &cobra.Command{
		Use:   "drift",
		Short: "Drift Module",
		Long:  "Drift Module",
	}

	driftScanCmd = &cobra.Command{
		Use:   "scan [state files...]",
		Short: "Scan for drifts",
		Long:  "Scan for drifts between cloud provider and IaC",
		Run: handleCommand(func(ctx context.Context, c *console.Client, cmd *cobra.Command, args []string) error {
			driftParams.StateFiles = args
			diags := c.CallModule(ctx, console.ModuleCallRequest{
				Name:       "drift",
				Params:     driftParams,
				Profile:    driftProfile,
				OutputPath: driftOutputPath,
			})
			analytics.Capture("drift", c.Providers, nil, diags)
			errors.CaptureError(diags, map[string]string{"command": "drift"})
			return diags
		}),
	}

	driftParams     drift.RunParams
	driftProfile    string
	driftOutputPath string
)

func init() {
	flags := driftScanCmd.Flags()

	// generic flags
	flags.StringVar(&driftOutputPath, "output", "", "Generate a new file at the given path with the output")
	flags.StringVar(&driftProfile, "profile", "", "Specify drift profile")

	// flags handled by the drift package
	flags.BoolVar(&driftParams.Debug, "debug", false, "Show debug output")
	flags.BoolVar(&driftParams.ForceDeep, "deep", false, "Force deep mode")
	flags.BoolVar(&driftParams.ListManaged, "list-managed", false, "List managed resources in output")

	driftCmd.SetUsageTemplate(usageTemplateWithFlags)
	driftCmd.AddCommand(driftScanCmd)
	rootCmd.AddCommand(driftCmd)
}
