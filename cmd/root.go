package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/endpoint"
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
	Short: "Build, run, test, and deploy services in a Codefly workspace",
	Long: `Codefly turns service development operations into consistent, agent-backed workflows.

Use it to create workspace resources, run and test services locally, build
container images, and deploy services to configured environments.`,
	Example: `  codefly init workspace my-project
  codefly add service api --agent=go-grpc
  codefly run service api
  codefly deploy service api --env=staging`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		return applyRootOptions()
	},
	Run: func(cmd *cobra.Command, args []string) {
		common.Logo()
	},
}

func init() {
	// Define a custom help template with color
	customHelpTemplate := color.New(color.FgCyan).Sprint("Usage:") + `
  {{.UseLine}}

{{if .Long}}{{.Long | trimTrailingWhitespaces}}{{else}}{{.Short | trimTrailingWhitespaces}}{{end}}

{{if .HasExample}}` + color.New(color.FgCyan).Sprint("Examples:") + `
{{.Example}}

{{end}}{{if .HasAvailableSubCommands}}` + color.New(color.FgCyan).Sprint("Available Commands:") + `
{{range .Commands}}{{if (and .IsAvailableCommand (not .IsAdditionalHelpTopicCommand))}}  {{rpad .Name .NamePadding }} {{.Short}}
{{end}}{{end}}

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

// Execute runs the root command and returns any failure to the process boundary.
func Execute() error {
	configureSubcommandValidation(RootCmd)
	return RootCmd.Execute()
}

func configureSubcommandValidation(command *cobra.Command) {
	for _, child := range command.Commands() {
		configureSubcommandValidation(child)
	}
	if command == RootCmd || !command.HasSubCommands() || command.Runnable() {
		return
	}
	command.Args = rejectUnknownSubcommand
	command.RunE = func(command *cobra.Command, _ []string) error {
		return command.Help()
	}
}

func rejectUnknownSubcommand(command *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	var available []string
	for _, child := range command.Commands() {
		if child.IsAvailableCommand() {
			available = append(available, child.Name())
		}
	}
	sort.Strings(available)
	if len(available) == 0 {
		return fmt.Errorf("unknown command %q for %q", args[0], command.CommandPath())
	}
	return fmt.Errorf("unknown command %q for %q\n\nAvailable subcommands:\n  %s",
		args[0], command.CommandPath(), strings.Join(available, "\n  "))
}

// IsMachineReadableError reports that a command already emitted its complete
// machine-readable failure payload. The process must still exit non-zero, but
// main must not append human diagnostics to stdout/stderr.
func IsMachineReadableError(err error) bool {
	var marker interface{ MachineReadable() bool }
	return errors.As(err, &marker) && marker.MachineReadable()
}

// applyRootOptions runs after Cobra has parsed the selected command and all of
// its local and persistent flags. This makes persistent options work regardless
// of whether they appear before or after subcommand-specific flags.
func applyRootOptions() error {
	if debug {
		wool.SetGlobalLogLevel(wool.DEBUG)
	}
	if trace {
		wool.SetGlobalLogLevel(wool.TRACE)
	}
	if focus {
		wool.SetGlobalLogLevel(wool.FOCUS)
	}
	cli.SetTimestamps(showTimestamps)
	if localAgents {
		// Propagate to the agent loader (core/agents/manager.AgentSourceLocal).
		// Setting via env so spawned subprocesses inherit it too.
		_ = os.Setenv("CODEFLY_AGENT_SOURCE", "local")
	}
	if pluginPath != "" {
		// Override ~/.codefly — subprocesses inherit via env.
		_ = os.Setenv(resources.CodeflyHomeEnv, pluginPath)
	}
	if tracker != "" {
		tr, err := actions.NewActionTracker(context.Background(), resources.CodeflyDir(), tracker)
		if err != nil {
			return fmt.Errorf("cannot create action tracker: %w", err)
		}
		actions.SetActionTracker(tr)
	}
	return nil
}

// Origin of the World
var (
	focus          bool
	debug          bool
	trace          bool
	tracker        string
	localAgents    bool
	pluginPath     string
	showTimestamps bool
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

	// Read-only inspection: dependency graph, network configuration, …
	RootCmd.AddCommand(ShowCmd)

	// Stop a running stack (reap processes + orphaned groups, keep containers).
	RootCmd.AddCommand(StopCmd)

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
	RootCmd.AddCommand(LintCmd)
	RootCmd.AddCommand(FixCmd)
	RootCmd.AddCommand(CompileCmd)
	RootCmd.AddCommand(PackageCmd)

	// Verify base-file integrity of composed modules
	RootCmd.AddCommand(VerifyCmd)

	// Audit + upgrade dependencies
	RootCmd.AddCommand(AuditCmd)
	RootCmd.AddCommand(SBOMCmd)
	RootCmd.AddCommand(UpgradeCmd)

	// CI
	RootCmd.AddCommand(CiCmd)

	// Daemon (background service management)
	RootCmd.AddCommand(DaemonCmd)

	// Durable per-user services managed by launchd or systemd.
	RootCmd.AddCommand(ServiceCmd)

	// Logs (show the CLI session logs)
	RootCmd.AddCommand(LogsCmd)

	// Terminal
	RootCmd.AddCommand(TerminalCmd)

	// Companion images (proto, language toolchains)
	RootCmd.AddCommand(CompanionCmd)

	// Unified release: bump + commit + tag + push for any codefly repo.
	RootCmd.AddCommand(PublishCmd)

	// Rebuild the codefly CLI itself from source.
	RootCmd.AddCommand(SelfCmd)

	// Query services/endpoints against the running daemon.
	RootCmd.AddCommand(GetCmd)

	// Resolve a single endpoint to bare host:port (script-friendly).
	RootCmd.AddCommand(endpoint.Cmd)

	// Static help plus optional workspace-aware AI guidance.
	RootCmd.AddCommand(ExplainCmd)

	RootCmd.PersistentFlags().BoolVar(&focus, "focus", false, "Enable focus log mode")
	RootCmd.PersistentFlags().BoolVarP(&debug, "debug", "d", false, "Enable debug mode")
	RootCmd.PersistentFlags().BoolVar(&trace, "trace", false, "Enable trace mode")
	RootCmd.PersistentFlags().BoolVar(&showTimestamps, "timestamps", true, "Prefix log output with a wall-clock timestamp (HH:MM:SS); use --timestamps=false to hide it")
	RootCmd.PersistentFlags().StringVar(&tracker, "track", "", "Tracker of actions -- advanced usage")
	RootCmd.PersistentFlags().BoolVar(&localAgents, "local-agents", false,
		"Resolve agent versions from ~/.codefly/agents/ only (skip GitHub). "+
			"Equivalent to setting CODEFLY_AGENT_SOURCE=local. Use when working "+
			"on local agent builds or offline.")
	RootCmd.PersistentFlags().StringVar(&pluginPath, "plugin-path", "",
		"Override the codefly home directory (default: ~/.codefly). Plugins, "+
			"containers and logs resolve from <plugin-path>/agents, "+
			"<plugin-path>/containers, <plugin-path>/logs. Equivalent to "+
			"setting CODEFLY_HOME.")
}
