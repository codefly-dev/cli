package cmd

import (
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/spf13/cobra"
)

// GenerateCmd represents the generate command
var GenerateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate code to access service",
}

func init() {
	GenerateCmd.AddCommand(generate.GRPCCmd)
}
