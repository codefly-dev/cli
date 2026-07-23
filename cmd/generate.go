package cmd

import (
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/spf13/cobra"
)

// GenerateCmd represents the generate command
var GenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate API clients or protobuf bindings from service contracts",
}

func init() {
	GenerateCmd.AddCommand(generate.GRPCCmd)
	GenerateCmd.AddCommand(generate.OpenAPICmd)
	GenerateCmd.AddCommand(generate.ProtoCmd)
}
