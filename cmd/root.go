package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	compositioncmd "github.com/codefly-dev/cli/cmd/composition"
	configcmd "github.com/codefly-dev/cli/cmd/config"
	"github.com/codefly-dev/cli/cmd/endpoint"
	"github.com/codefly-dev/cli/cmd/environment"
	providercmd "github.com/codefly-dev/cli/cmd/provider"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/actions/actions"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/fatih/color"

	"github.com/spf13/cobra"
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:           "codefly",
	Short:         "Build, run, test, and deploy services in a Codefly workspace",
	SilenceErrors: true,
	SilenceUsage:  true,
	Long: `Codefly turns service development operations into consistent, agent-backed workflows.

Use it to create workspace resources, run and test services locally, build
container images, and deploy services to configured environments.`,
	Example: `  codefly init workspace my-project
  codefly add service api --agent=go-grpc
  codefly run service api
  codefly deploy service api --env=staging`,
	PersistentPreRunE: func(_ *cobra.Command, args []string) error {
		return applyRootOptions()
	},
	Run: func(_ *cobra.Command, args []string) {
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

// IsCancellationError reports a graceful interruption initiated by the
// operator. The process boundary suppresses ordinary failure rendering for
// this case: the terminal already echoed Ctrl+C, and usage/error/log hints do
// not help the operator recover.
func IsCancellationError(err error) bool {
	return errors.Is(err, context.Canceled)
}

// ShouldRenderError reports whether the process boundary should emit the
// human-readable error chain. Cancellation is already visible as Ctrl+C, and
// machine-readable commands own their complete output.
func ShouldRenderError(err error) bool {
	return !IsCancellationError(err) && !IsMachineReadableError(err)
}

// ExitCode returns the conventional process status for a command error.
// Commands may carry a stable, documented status by implementing
// CommandExitCode() on their error (see pkg/provider for the provider command
// contract). The method name is deliberately distinctive: a bare ExitCode()
// would also match os/exec.ExitError (promoted from os.ProcessState), which
// would leak a wrapped subprocess's exit code into unrelated commands.
func ExitCode(err error) int {
	if IsCancellationError(err) {
		return 130
	}
	var coded interface{ CommandExitCode() int }
	if errors.As(err, &coded) {
		return coded.CommandExitCode()
	}
	return 1
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
	markLaunchWorkspace()
	return nil
}

// markLaunchWorkspace records which workspace this invocation's run belongs to,
// before it starts anything. Everything the run goes on to spawn inherits it —
// agents, the runners inside them, service binaries, the stores they bring up —
// so `stop` and `ps` can ask a process whose run it is instead of guessing from
// where it happens to be running.
//
// The guess is what they used to do, and it is wrong for exactly the processes a
// composed run adds: a composed module's service runs from that module's
// checkout, which sits outside the workspace that composed it, and a store runs
// from a data directory that sits in no workspace at all. Both then read as
// belonging to nobody, so a scoped stop skipped them and reported success while
// they kept holding their ports.
//
// Outside a workspace there is nothing to record and any inherited value stands,
// which is what keeps a codefly invoked from inside a run attributed to that run.
func markLaunchWorkspace() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	markLaunchWorkspaceFrom(cwd)
}

// markLaunchWorkspaceFrom records the workspace enclosing dir, for a command
// that serves a workspace it was pointed at rather than the one it was started
// in. It re-records deliberately: the directory the command was given is a
// better answer than the directory it happens to be standing in.
func markLaunchWorkspaceFrom(dir string) {
	workspace, ok := processgroup.CurrentWorkspace(dir)
	if !ok {
		return
	}
	processgroup.MarkLaunchWorkspace(workspace)
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
	RootCmd.AddCommand(OverrideCmd)
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

	// Inspect running processes
	RootCmd.AddCommand(PsCmd)

	// Test things
	RootCmd.AddCommand(TestCmd)
	RootCmd.AddCommand(LintCmd)
	RootCmd.AddCommand(FixCmd)
	RootCmd.AddCommand(CompileCmd)
	RootCmd.AddCommand(PackageCmd)

	// Verify base-file integrity of composed modules
	RootCmd.AddCommand(compositioncmd.NewCommand())

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

	// Check release status: versions, health, auto-create issues.
	RootCmd.AddCommand(StatusCmd)

	// Rebuild the codefly CLI itself from source.
	RootCmd.AddCommand(SelfCmd)

	// Query services/endpoints against the running daemon.
	RootCmd.AddCommand(GetCmd)

	// Resolve a single endpoint to bare host:port (script-friendly).
	RootCmd.AddCommand(endpoint.Cmd)

	// Provision the configuration files Codefly reads. Configuration is
	// Codefly's format, so writing it is a Codefly operation — consumers must
	// not hand-assemble configurations/<profile>/*.env themselves.
	RootCmd.AddCommand(configcmd.Cmd)
	RootCmd.AddCommand(providercmd.Cmd)

	// Declare and inspect deploy environments (import a coordinate contract, show).
	RootCmd.AddCommand(environment.Cmd)

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
