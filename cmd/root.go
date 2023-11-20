package cmd

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/context"
	"github.com/codefly-dev/core/shared"

	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "codefly",
	Short: "🪄Codefly is magic",
	Run: func(cmd *cobra.Command, args []string) {
		common.Logo()
		context.ShowCurrent()
	},
}

// Execute adds codefly-sdk child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	_ = RootCmd.ParseFlags(os.Args)
	shared.SetDebug(debug)
	shared.SetTrace(trace)
	shared.SetTodo(todo)
	shared.SetOverride(override)

	shared.ExitOnError(RootCmd.Execute(), "cannot execute command")
}

// Origin of the World
var (
	root     string
	debug    bool
	todo     bool
	trace    bool
	override bool
)

func init() {
	// Auto-completion
	RootCmd.AddCommand(CompletionCmd)

	// Version
	RootCmd.AddCommand(VersionCmd)

	// Initialization and configuration
	RootCmd.AddCommand(InitCmd)
	RootCmd.AddCommand(ContextCmd)
	RootCmd.AddCommand(ListCmd)

	// Import
	RootCmd.AddCommand(ImportCmd)

	// Add, Update and Sync
	RootCmd.AddCommand(AddCmd)
	RootCmd.AddCommand(UpdateCmd)
	RootCmd.AddCommand(SyncCmd)

	// Delete
	RootCmd.AddCommand(DeleteCmd)

	// Installation
	RootCmd.AddCommand(InstallCmd)

	// View
	RootCmd.AddCommand(ViewCmd)

	// Run
	RootCmd.AddCommand(RunCmd)

	// Build
	RootCmd.AddCommand(BuildCmd)

	// Deploy
	RootCmd.AddCommand(DeployCmd)

	// Open your applications in your favorite editor
	RootCmd.AddCommand(OpenCmd)

	// Plugins
	RootCmd.AddCommand(PluginCmd)

	RootCmd.PersistentFlags().StringVar(&root, "root", "", "NewDir directory of the project")
	RootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug mode")
	RootCmd.PersistentFlags().BoolVar(&trace, "trace", false, "Enable trace mode")
	RootCmd.PersistentFlags().BoolVar(&todo, "todo", false, "Print TODOs")
	RootCmd.PersistentFlags().BoolVar(&override, "override", false, "Override all")
	RootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
