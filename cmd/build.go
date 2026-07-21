package cmd

import (
	"github.com/codefly-dev/cli/cmd/build"
	"github.com/spf13/cobra"
)

// BuildCmd represents the build command
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build container images for a service or an entire module",
}

func init() {
	BuildCmd.AddCommand(build.ServiceCmd)
	BuildCmd.AddCommand(build.ModuleCmd)
}
