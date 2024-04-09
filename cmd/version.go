package cmd

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// VersionCmd represents the build command
var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Version of codefly",
	Run: func(cmd *cobra.Command, args []string) {
		version, err := cli.GetCurrentVersion()
		cli.ExitOnError(err, "Cannot get current version")
		cmd.Println(version)
	},
}
