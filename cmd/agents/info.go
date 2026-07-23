package agents

import (
	"github.com/codefly-dev/cli/cmd/agents/kinds"
	"github.com/spf13/cobra"
)

// InfoCmd represents the run command
var InfoCmd = &cobra.Command{
	Use:   "info",
	Short: "Inspect metadata reported by an installed service agent",
}

func init() {
	InfoCmd.AddCommand(kinds.ServiceCmd)
}
