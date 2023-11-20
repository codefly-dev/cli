package cmd

import (
	"github.com/spf13/cobra"
)

// VersionCmd represents the build command
var VersionCmd = &cobra.Command{
	Use:   "version",
	Short: "Version of codefly",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Println("0.0.1")
	},
}
