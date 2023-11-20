package cmd

import (
	"github.com/codefly-dev/cli/cmd/build"
	"github.com/spf13/cobra"
)

// BuildCmd represents the build command
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build of your applications",
}

func init() {
	BuildCmd.AddCommand(build.PartialCmd)
	BuildCmd.AddCommand(build.ApplicationCmd)
	BuildCmd.AddCommand(build.ServiceCmd)
}
