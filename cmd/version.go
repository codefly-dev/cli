package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// VersionCmd represents the build command
var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the installed Codefly CLI version",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		version, err := cli.GetCurrentVersion()
		if err != nil {
			return fmt.Errorf("cannot get current version: %w", err)
		}
		if versionJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(cli.GetBuildInfo())
		}
		cmd.Println(version)
		return nil
	},
}

var versionJSON bool

func init() {
	VersionCmd.Flags().BoolVar(&versionJSON, "json", false, "Print version, commit, and build date as JSON")
}
