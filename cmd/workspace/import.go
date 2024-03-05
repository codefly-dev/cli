package workspace

import (
	"github.com/spf13/cobra"
)

// ImportCmd represents the run command
var ImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Import to workspace",
	Run: func(cmd *cobra.Command, args []string) {
	},
}

func ImportToWorkspace() {
	// TODO
}
