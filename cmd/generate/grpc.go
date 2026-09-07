package generate

import (
	"github.com/spf13/cobra"
)

// GRPCCmd is a hidden, deprecated alias for
// `generate client --language <lang> --no-facade --output <destination>`.
// generate grpc/openapi are orphaned (no caller in any surveyed repo) and
// produced loose files instead of a codefly library; generate client
// replaces them.
var GRPCCmd = &cobra.Command{
	Use:     "grpc",
	Aliases: []string{"gRPC"},
	Hidden:  true,
	Short:   "Deprecated: use `generate client --no-facade` instead",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLegacyClientAlias(cmd, "grpc")
	},
}

func init() {
	GRPCCmd.Flags().StringVar(&serviceInput, "service", "", "service to generate gRPC client code for")
	GRPCCmd.Flags().StringVar(&languageInput, "language", "go", "languageInput to generate gRPC client code in")
	GRPCCmd.Flags().StringVar(&destination, "destination", "", "destination for the client")
}
