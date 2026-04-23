package cmd

import (
	"context"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/actions/actions"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/fatih/color"

	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "codefly",
	Short: "🪄Codefly is magic",
	Run: func(cmd *cobra.Command, args []string) {
		common.Logo()
	},
}

func init() {
	// Define a custom help template with color
	customHelpTemplate := color.New(color.FgCyan).Sprint("Usage:") + `
{{.UseLine}}

{{if .Long}}{{.Long | trimTrailingWhitespaces}}

{{end}}{{if .HasExample}}` + color.New(color.FgCyan).Sprint("Examples:") + `
{{.Example}}

{{end}}{{if .HasAvailableSubCommands}}` + color.New(color.FgCyan).Sprint("Available Commands:") + `
{{range .Commands}}{{if (and .IsAvailableCommand (not .IsAdditionalHelpTopicCommand))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

{{end}}{{if .HasAvailableLocalFlags}}` + color.New(color.FgCyan).Sprint("Flags:") + `
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}

{{end}}{{if .HasAvailableInheritedFlags}}` + color.New(color.FgCyan).Sprint("Global Flags:") + `
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}

{{end}}{{if .HasHelpSubCommands}}` + color.New(color.FgCyan).Sprint("Additional help topics:") + `
{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}

{{end}}
`

	// Set the custom help template
	RootCmd.SetHelpTemplate(customHelpTemplate)
}

// Execute adds codefly-sdk child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	_ = RootCmd.ParseFlags(os.Args)
	if debug {
		wool.SetGlobalLogLevel(wool.DEBUG)
	}
	if trace {
		wool.SetGlobalLogLevel(wool.TRACE)
	}
	if focus {
		wool.SetGlobalLogLevel(wool.FOCUS)
	}
	if localAgents {
		// Propagate to the agent loader (core/agents/manager.AgentSourceLocal).
		// Setting via env so spawned subprocesses inherit it too.
		_ = os.Setenv("CODEFLY_AGENT_SOURCE", "local")
	}
	if tracker != "" {
		tr, err := actions.NewActionTracker(context.Background(), resources.CodeflyDir(), tracker)
		cli.ExitOnError(err, "cannot create action tracker")
		actions.SetActionTracker(tr)
	}

	cli.ExitOnError(RootCmd.Execute(), "cannot execute command")
}

// Origin of the World
var (
	focus       bool
	debug       bool
	trace       bool
	tracker     string
	localAgents bool
)

func init() {
	// Auto-completion
	RootCmd.AddCommand(CompletionCmd)

	// Server
	RootCmd.AddCommand(ServerCmd)

	// Version
	RootCmd.AddCommand(VersionCmd)

	// Initialization and configuration
	RootCmd.AddCommand(LoginCmd)

	RootCmd.AddCommand(ListCmd)

	// Generate client code
	RootCmd.AddCommand(GenerateCmd)

	// Import
	RootCmd.AddCommand(ImportCmd)

	// New, Add, Update and Sync
	RootCmd.AddCommand(InitCmd)
	RootCmd.AddCommand(AddCmd)
	RootCmd.AddCommand(UpdateCmd)
	RootCmd.AddCommand(SyncCmd)

	// Delete
	RootCmd.AddCommand(DeleteCmd)

	// Installation
	RootCmd.AddCommand(InstallCmd)

	// Begin
	RootCmd.AddCommand(RunCmd)

	// Build
	RootCmd.AddCommand(BuildCmd)

	// Handle
	RootCmd.AddCommand(DeployCmd)

	// Open your modules in your favorite editor
	RootCmd.AddCommand(OpenCmd)

	// Agents
	RootCmd.AddCommand(AgentCmd)

	// MCP Server
	RootCmd.AddCommand(MCPCmd)

	// Replay
	RootCmd.AddCommand(ReplayCmd)

	// Expose for local k8s development
	RootCmd.AddCommand(ExposeCmd)

	// Clear things
	RootCmd.AddCommand(ClearCmd)

	// Test things
	RootCmd.AddCommand(TestCmd)

	// Audit + upgrade dependencies
	RootCmd.AddCommand(AuditCmd)
	RootCmd.AddCommand(UpgradeCmd)

	// CI
	RootCmd.AddCommand(CiCmd)

	// Daemon (background service management)
	RootCmd.AddCommand(DaemonCmd)

	// Terminal
	RootCmd.AddCommand(TerminalCmd)

	RootCmd.PersistentFlags().BoolVar(&focus, "focus", false, "Enable focus log mode")
	RootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug mode")
	RootCmd.PersistentFlags().BoolVar(&trace, "trace", false, "Enable trace mode")
	RootCmd.PersistentFlags().StringVar(&tracker, "track", "", "Tracker of actions -- advanced usage")
	RootCmd.PersistentFlags().BoolVar(&localAgents, "local-agents", false,
		"Resolve agent versions from ~/.codefly/agents/ only (skip GitHub). "+
			"Equivalent to setting CODEFLY_AGENT_SOURCE=local. Use when working "+
			"on local agent builds or offline.")
}
