package provider

import (
	"context"
	"strings"

	"github.com/spf13/cobra"
)

var importCmd = &cobra.Command{
	Use:          "import BINDING TYPE REMOTE_ID",
	Short:        "Adopt an existing remote resource into a binding by exact identity",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(3),
	Long: `Adopt an existing remote resource into a binding.

Adoption is exact and explicit: it requires the resource TYPE and its exact
REMOTE_ID. URL, name, domain, or email similarity never adopts a resource.`,
	RunE: run(func(ctx context.Context, args []string) error {
		binding, resourceType, remoteID := args[0], strings.TrimSpace(args[1]), strings.TrimSpace(args[2])
		if resourceType == "" || remoteID == "" {
			return errImportIdentity()
		}
		return gatedCommand(ctx, "import", envFlag, binding)
	}),
}

func init() {
	importCmd.Flags().StringVar(&envFlag, "env", "local", "Environment the binding belongs to")
	importCmd.Flags().BoolVar(&jsonFlag, "json", false, "Print a machine-readable report to stdout")
}
