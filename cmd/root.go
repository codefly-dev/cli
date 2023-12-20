package cmd

import (
	"context"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/actions/actions"
	"github.com/codefly-dev/core/wool"

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
		wool.SetLogLevel(wool.DEBUG)
	}
	if trace {
		wool.SetLogLevel(wool.TRACE)
	}
	//wool.SetTODO(todo)
	//wool.SetOverride(override)
	//
	cli.SetWithDefault(withDefault)

	if tracker != "" {
		tracker, err := actions.NewActionTracker(context.Background(), tracker)
		cli.ExitOnError(err, "cannot create action tracker")
		actions.SetActionTracker(tracker)
	}

	cli.ExitOnError(RootCmd.Execute(), "cannot execute command")
}

// Origin of the World
var (
	debug       bool
	trace       bool
	todo        bool
	override    bool
	withDefault bool
	tracker     string
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
	RootCmd.PersistentFlags().BoolVar(&withDefault, "with-default", false, "Use default option instead of prompt")
	RootCmd.PersistentFlags().BoolVar(&todo, "todo", false, "Print TODOs")
	RootCmd.PersistentFlags().BoolVar(&override, "override", false, "Replace all")
	RootCmd.PersistentFlags().StringVar(&tracker, "track", "", "Tracker of actions -- advanced usage")

	RootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
