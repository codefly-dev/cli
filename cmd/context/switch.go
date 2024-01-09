package context

import (
	switch_ "github.com/codefly-dev/cli/cmd/context/switch"
	"github.com/spf13/cobra"
)

// SwitchCmd represents the run command
var SwitchCmd = &cobra.Command{
	Use:   "switch",
	Short: "Switch active context",
	Run: func(cmd *cobra.Command, args []string) {
		switch_.Service()
	},
}

func init() {
	SwitchCmd.AddCommand(switch_.ProjectCmd)
	SwitchCmd.AddCommand(switch_.ApplicationCmd)
	SwitchCmd.AddCommand(switch_.ServiceCmd)
}
