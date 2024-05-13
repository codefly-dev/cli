package cmd

import (
	"github.com/codefly-dev/cli/cmd/test"
	"github.com/spf13/cobra"
)

// TestCmd represents the test command
var TestCmd = &cobra.Command{
	Use:   "test",
	Short: "Test",
}

func init() {
	TestCmd.AddCommand(test.ServiceCmd)
}
