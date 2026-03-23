package cmd

import (
	"github.com/codefly-dev/cli/cmd/agents"
	"github.com/spf13/cobra"
)

// AgentCmd represents the build command
var AgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "agent commands",
}

func init() {
	AgentCmd.AddCommand(agents.InfoCmd)
	AgentCmd.AddCommand(agents.GenerateCmd)
	AgentCmd.AddCommand(agents.BuildCmd)
}
