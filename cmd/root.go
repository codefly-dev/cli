package cmd

import (
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/actions/actions"
	"github.com/codefly-dev/core/shared"

	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "codefly",
	Short: "🪄Codefly is magic",
	Run: func(cmd *cobra.Command, args []string) {
		common.Logo()
		//context.ShowCurrent()
	},
}

// Execute adds codefly-sdk child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	_ = RootCmd.ParseFlags(os.Args)
	if debug {
		shared.SetLogLevel(shared.Debug)
	}
	if trace {
		shared.SetLogLevel(shared.Trace)
	}
	shared.SetTodo(todo)
	shared.SetOverride(override)

	if tracker != "" {
		actions.InitActionTracker(tracker)
	}

	shared.ExitOnError(RootCmd.Execute(), "cannot execute command")
}

// Origin of the World
var (
	debug    bool
	todo     bool
	trace    bool
	override bool
	tracker  string
)

func init() {
	// Auto-completion
	RootCmd.AddCommand(CompletionCmd)

	// Server
	RootCmd.AddCommand(ServerCmd)

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

	// Run
	RootCmd.AddCommand(RunCmd)

	// Build
	RootCmd.AddCommand(BuildCmd)

	// Deploy
	RootCmd.AddCommand(DeployCmd)

	// Open your applications in your favorite editor
	RootCmd.AddCommand(OpenCmd)

	// Agents
	RootCmd.AddCommand(AgentCmd)

	// Replay
	RootCmd.AddCommand(ReplayCmd)

	RootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug mode")
	RootCmd.PersistentFlags().BoolVar(&trace, "trace", false, "Enable trace mode")
	RootCmd.PersistentFlags().BoolVar(&todo, "todo", false, "Print TODOs")
	RootCmd.PersistentFlags().BoolVar(&override, "override", false, "Replace all")
	RootCmd.PersistentFlags().StringVar(&tracker, "track", "", "Tracker of actions -- advanced usage")
	RootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
