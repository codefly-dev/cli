package cmd

import (
	"github.com/codefly-dev/cli/cmd/test"
	"github.com/spf13/cobra"
)

// TestCmd represents the test command
var TestCmd = &cobra.Command{
	Use:   "test",
	Short: "Run plugin-owned tests for a service or source checkout",
}

func init() {
	TestCmd.AddCommand(test.ServiceCmd)
	TestCmd.AddCommand(test.SourceCmd)
}
