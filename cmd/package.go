package cmd

import (
	packageartifact "github.com/codefly-dev/cli/cmd/packageartifact"
	"github.com/spf13/cobra"
)

// PackageCmd exposes portable, plugin-owned source packaging.
var PackageCmd = &cobra.Command{
	Use:   "package",
	Short: "Create portable service artifacts with a Codefly plugin",
}

func init() {
	PackageCmd.AddCommand(packageartifact.ServiceCmd)
}
