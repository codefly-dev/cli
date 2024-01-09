package cmd

import (
	"github.com/codefly-dev/cli/cmd/build"
	"github.com/spf13/cobra"
)

// BuildCmd represents the build command
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Build your containers",
}

func init() {
	BuildCmd.AddCommand(build.ServiceCmd)
}
