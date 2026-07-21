package cmd

import (
	"github.com/codefly-dev/cli/cmd/install"
	"github.com/spf13/cobra"
)

// InstallCmd represents the install command
var InstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install reusable components into the current workspace",
}

func init() {
	InstallCmd.AddCommand(install.LibraryCmd)
}
