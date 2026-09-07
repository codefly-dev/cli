package generate

import (
	"github.com/spf13/cobra"
)

// OpenAPICmd is a hidden, deprecated alias for
// `generate client --language <lang> --no-facade --output <destination>`.
// generate grpc/openapi are orphaned (no caller in any surveyed repo) and
// produced loose files instead of a codefly library; generate client
// replaces them.
var OpenAPICmd = &cobra.Command{
	Use:     "openapi",
	Aliases: []string{"openAPI", "swagger"},
	Hidden:  true,
	Short:   "Deprecated: use `generate client --no-facade` instead",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runLegacyClientAlias(cmd, "openapi")
	},
}

func init() {
	OpenAPICmd.Flags().StringVar(&serviceInput, "service", "", "service to generate openAPI client code for")
	OpenAPICmd.Flags().StringVar(&languageInput, "language", "go", "languageInput to generate openAPI client code in")
	OpenAPICmd.Flags().StringVar(&destination, "destination", "", "destination for the client")
}
