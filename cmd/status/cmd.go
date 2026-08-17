package status

import (
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "status",
	Short: "Check system status (releases, agents, health)",
}

func init() {
	Cmd.AddCommand(releaseCmd)
}
