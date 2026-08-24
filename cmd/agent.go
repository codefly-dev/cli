package cmd

import (
	"github.com/codefly-dev/cli/cmd/agents"
	"github.com/spf13/cobra"
)

// AgentCmd represents the build command
var AgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Develop, inspect, install, and validate Codefly service agents",
}

func init() {
	AgentCmd.AddCommand(agents.InfoCmd)
	AgentCmd.AddCommand(agents.GenerateCmd)
	AgentCmd.AddCommand(agents.BuildCmd)
	AgentCmd.AddCommand(agents.AgentCICmd)
	AgentCmd.AddCommand(agents.DepsCmd)
	AgentCmd.AddCommand(agents.InstallCmd)
	AgentCmd.AddCommand(agents.VersionsCmd)
	AgentCmd.AddCommand(agents.ListCmd)
	AgentCmd.AddCommand(agents.PromoteSourceCmd)
	AgentCmd.AddCommand(agents.VerifyPlatformCmd)
}
