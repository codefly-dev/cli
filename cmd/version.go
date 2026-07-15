package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// VersionCmd represents the build command
var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Version of codefly",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		version, err := cli.GetCurrentVersion()
		if err != nil {
			return fmt.Errorf("cannot get current version: %w", err)
		}
		cmd.Println(version)
		return nil
	},
}
