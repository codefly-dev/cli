package cmd

import (
	"github.com/codefly-dev/cli/cmd/imports"
	"github.com/spf13/cobra"
)

// ImportCmd represents the imports command
var ImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import existing source code as Codefly workspace resources",
}

func init() {
	ImportCmd.AddCommand(imports.ModuleCmd)
}
