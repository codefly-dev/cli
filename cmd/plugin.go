package cmd

import (
	"github.com/codefly-dev/cli/cmd/plugins"
	"github.com/spf13/cobra"
)

// PluginCmd represents the build command
var PluginCmd = &cobra.Command{
	Use:   "plugin",
	Short: "plugin commands",
}

func init() {
	PluginCmd.AddCommand(plugins.GenerateCmd)
}
