package run

// ARCHITECTURE: `codefly run command` is the one-shot process boundary for
// workspace tooling that needs a service's real dependency context. Core's
// sdk.WithDependencies remains the single owner of graph startup, readiness,
// typed endpoint/configuration injection, and teardown; this command only
// composes that lifecycle with an operator-supplied argv. It never interprets
// a language or framework command and never invokes a shell.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/resources"
	coresdk "github.com/codefly-dev/core/sdk"
	"github.com/spf13/cobra"
)

var (
	commandDependencyTimeout time.Duration
	commandNamingScope       string
	commandFixture           string
	commandProfile           string
	commandSilent            []string
	commandExcluded          []string
)

// CommandCmd runs an explicit argv after Codefly has started the current
// service's declared dependencies and injected their production environment.
var CommandCmd = &cobra.Command{
	Use:   "command -- <program> [args...]",
	Short: "Run a command with the current service's dependencies",
	Long: `Run an explicit one-shot command inside the current service's managed
dependency context.

Codefly starts the service's declared dependencies, waits for readiness,
injects typed endpoints and configurations, executes argv directly, and tears
the owned dependency flow down when the command exits. Use -- before the
program so every following flag belongs to the child command.

Examples:
	codefly run command -- ./scripts/verify
  codefly run command -- ./bin/maintenance --once`,
	Args: validateManagedCommandArgs,
	RunE: func(_ *cobra.Command, args []string) error {
		return runManagedCommand(args)
	},
}

func validateManagedCommandArgs(cmd *cobra.Command, args []string) error {
	if cmd.ArgsLenAtDash() != 0 {
		return fmt.Errorf("put the command after --: codefly run command -- <program> [args...]")
	}
	return cobra.MinimumNArgs(1)(cmd, args)
}

func runManagedCommand(argv []string) (returnErr error) {
	ctx, done := common.NewContext()
	defer done()
	ctx, stopSignals := common.SignalContext(ctx)
	defer stopSignals()

	// ARCHITECTURE: Resolve the service before the Core SDK creates its nested
	// headless `run service` process. A managed command has no interactive
	// service-selection phase: the current path (or a deterministic module
	// entry/single-service workspace) must identify the service. Failing here
	// preserves the actionable candidate list and, critically, prevents the
	// background process group from ever opening /dev/tty.
	if err := preflightManagedCommandService(ctx); err != nil {
		return err
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current Codefly executable: %w", err)
	}
	options := managedCommandDependencyOptions(executable)

	dependencies, err := coresdk.WithDependencies(ctx, options...)
	if err != nil {
		return fmt.Errorf("start command dependencies: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		returnErr = errors.Join(returnErr, dependencies.Destroy(cleanupCtx))
	}()

	return executeManagedCommand(ctx, argv)
}

func preflightManagedCommandService(ctx context.Context) error {
	if _, _, _, err := common.LoadRequiredNonInteractiveE(ctx, nil); err != nil {
		return fmt.Errorf("resolve current service for managed command: %w", err)
	}
	return nil
}

// managedCommandDependencyOptions binds nested dependency startup to the
// invoking CLI release. The SDK remains the lifecycle owner, while PATH and a
// caller environment cannot silently compose two different CLI versions.
func managedCommandDependencyOptions(executable string) []coresdk.OptionFunc {
	options := []coresdk.OptionFunc{
		coresdk.WithTimeout(commandDependencyTimeout),
		coresdk.WithCodeflyBinary(executable),
	}
	if commandNamingScope != "" {
		options = append(options, coresdk.WithNamingScope(commandNamingScope))
	}
	if commandFixture != "" {
		options = append(options, coresdk.WithFixture(commandFixture))
	}
	if commandProfile != "" {
		options = append(options, coresdk.WithRunProfile(commandProfile))
	}
	if len(commandSilent) > 0 {
		options = append(options, coresdk.WithSilence(commandSilent...))
	}
	if len(commandExcluded) > 0 {
		options = append(options, coresdk.WithExcludedDependencies(commandExcluded...))
	}

	return options
}

// executeManagedCommand preserves the terminal streams and invokes argv
// directly. A typed wrapper deliberately carries the child status through the
// CLI process boundary; unrelated subprocess failures retain the CLI's generic
// exit code contract.
func executeManagedCommand(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return errors.New("managed command argv is empty")
	}
	//nolint:gosec // G204/G702: this boundary intentionally executes the
	// operator's explicit argv. No shell parses or expands it before execution.
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = withManagedRuntimeMarker(os.Environ())
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &managedCommandExitError{program: argv[0], code: exitErr.ExitCode(), err: err}
		}
		return fmt.Errorf("start %q: %w", argv[0], err)
	}
	return nil
}

// withManagedRuntimeMarker makes the child boundary indistinguishable from a
// process started by a Codefly service agent. In particular, an SDK consumer
// inside the command reuses these live dependencies instead of recursively
// starting a second stack.
func withManagedRuntimeMarker(environment []string) []string {
	prefix := resources.RunningPrefix + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+"true")
}

type managedCommandExitError struct {
	program string
	code    int
	err     error
}

func (err *managedCommandExitError) Error() string {
	return fmt.Sprintf("command %q exited with status %d", err.program, err.code)
}

func (err *managedCommandExitError) Unwrap() error {
	return err.err
}

func (err *managedCommandExitError) CommandExitCode() int {
	return err.code
}

func init() {
	CommandCmd.Flags().DurationVar(&commandDependencyTimeout, "dependency-timeout", 2*time.Minute, "Maximum time to wait for dependencies to become ready")
	CommandCmd.Flags().StringVar(&commandNamingScope, "naming-scope", "", "Runtime naming scope for dependency isolation")
	CommandCmd.Flags().StringVar(&commandFixture, "fixture", "", "Fixture override for the dependency flow")
	CommandCmd.Flags().StringVar(&commandProfile, "profile", "", "Named workspace run profile")
	CommandCmd.Flags().StringSliceVar(&commandSilent, "silent", nil, "Silence dependency services in CLI output")
	CommandCmd.Flags().StringSliceVar(&commandExcluded, "exclude-dependency", nil, "Exclude optional dependency services (repeatable)")
}
