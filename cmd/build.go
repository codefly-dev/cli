package cmd

import (
	"github.com/codefly-dev/cli/cmd/build"
	"github.com/spf13/cobra"
)

// BuildCmd represents the build command
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build service/module images or a native Runnable package",
}

func init() {
	BuildCmd.AddCommand(build.ServiceCmd)
	BuildCmd.AddCommand(build.ModuleCmd)
	BuildCmd.AddCommand(build.RunnableCmd)
}
