package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

func TestRootOwnsErrorRendering(t *testing.T) {
	if !RootCmd.SilenceErrors {
		t.Fatal("root command allows Cobra to duplicate error rendering")
	}
	if !RootCmd.SilenceUsage {
		t.Fatal("root command prints usage for runtime failures")
	}
}

func TestCancellationExitBehavior(t *testing.T) {
	err := fmt.Errorf("wrapped interruption: %w", context.Canceled)
	if !IsCancellationError(err) {
		t.Fatalf("IsCancellationError(%v) = false", err)
	}
	if ShouldRenderError(err) {
		t.Fatalf("ShouldRenderError(%v) = true", err)
	}
	if got := ExitCode(err); got != 130 {
		t.Fatalf("ExitCode(cancellation) = %d, want 130", got)
	}

	ordinary := errors.New("boom")
	if IsCancellationError(ordinary) {
		t.Fatalf("IsCancellationError(%v) = true", ordinary)
	}
	if !ShouldRenderError(ordinary) {
		t.Fatalf("ShouldRenderError(%v) = false", ordinary)
	}
	if got := ExitCode(ordinary); got != 1 {
		t.Fatalf("ExitCode(ordinary error) = %d, want 1", got)
	}
}

func TestCommandDescriptionsAreUseful(t *testing.T) {
	placeholder := map[string]bool{
		"add": true, "agent commands": true, "ci": true, "delete": true,
		"handle": true, "import": true, "init": true, "install": true,
		"list": true, "replay": true, "test": true, "update": true,
	}
	var visit func(*cobra.Command)
	visit = func(command *cobra.Command) {
		short := strings.TrimSpace(command.Short)
		name := strings.ToLower(command.Name())
		normalized := strings.ToLower(short)
		switch {
		case short == "":
			t.Errorf("%s has an empty Short description", command.CommandPath())
		case len(strings.Fields(short)) == 1:
			t.Errorf("%s has a single-word Short description %q", command.CommandPath(), short)
		case normalized == name || normalized == name+" commands":
			t.Errorf("%s has a name-echoing Short description %q", command.CommandPath(), short)
		case placeholder[normalized]:
			t.Errorf("%s has placeholder Short description %q", command.CommandPath(), short)
		}
		children := make(map[string]bool)
		for _, child := range command.Commands() {
			if children[child.Name()] {
				t.Errorf("%s registers subcommand %q more than once", command.CommandPath(), child.Name())
			}
			children[child.Name()] = true
			visit(child)
		}
	}
	visit(RootCmd)
}

func TestUnknownNestedSubcommandListsAvailableCommands(t *testing.T) {
	configureSubcommandValidation(RootCmd)
	var output bytes.Buffer
	previousOut := RootCmd.OutOrStdout()
	previousErr := RootCmd.ErrOrStderr()
	previousSilenceErrors := RootCmd.SilenceErrors
	defer func() {
		RootCmd.SetArgs(nil)
		RootCmd.SetOut(previousOut)
		RootCmd.SetErr(previousErr)
		RootCmd.SilenceErrors = previousSilenceErrors
	}()
	RootCmd.SetArgs([]string{"build", "agent"})
	RootCmd.SetOut(&output)
	RootCmd.SetErr(&output)
	RootCmd.SilenceErrors = true

	err := RootCmd.Execute()
	if err == nil {
		t.Fatal("unknown nested subcommand succeeded")
	}
	for _, expected := range []string{
		`unknown command "agent" for "codefly build"`,
		"Available subcommands:",
		"module",
		"service",
	} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("error %q does not contain %q", err, expected)
		}
	}
}

func TestRejectUnknownSubcommandWithoutAvailableChildren(t *testing.T) {
	command := &cobra.Command{Use: "empty"}
	err := rejectUnknownSubcommand(command, []string{"child"})
	if got, want := fmt.Sprint(err), `unknown command "child" for "empty"`; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestLeafHelpIncludesShortDescription(t *testing.T) {
	target, err := findExplainTarget(RootCmd, []string{"build", "service"})
	if err != nil {
		t.Fatal(err)
	}
	help, err := renderCommandHelp(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help, "Build a service container image for a target environment") {
		t.Fatalf("help omitted the command description:\n%s", help)
	}
}

func TestPersistentFlagAfterSubcommandFlagIsApplied(t *testing.T) {
	oldLevel := wool.GlobalLogLevel()
	oldDebug := debug
	defer func() {
		wool.SetGlobalLogLevel(oldLevel)
		debug = oldDebug
		_ = RootCmd.PersistentFlags().Set("debug", "false")
		RootCmd.SetArgs(nil)
	}()

	var observedLevel wool.Loglevel
	probe := &cobra.Command{
		Use: "root-flag-probe",
		Run: func(cmd *cobra.Command, args []string) {
			observedLevel = wool.GlobalLogLevel()
		},
	}
	probe.Flags().Int("number", 0, "subcommand-local test flag")
	RootCmd.AddCommand(probe)
	defer RootCmd.RemoveCommand(probe)

	debug = false
	_ = RootCmd.PersistentFlags().Set("debug", "false")
	RootCmd.SetArgs([]string{"root-flag-probe", "--number", "1", "--debug"})
	if err := RootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if observedLevel != wool.DEBUG {
		t.Fatalf("global log level = %v, want DEBUG", observedLevel)
	}
}

func TestSimpleRootCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{LoginCmd, VersionCmd, VerifyCmd} {
		if command.RunE == nil || command.Run != nil {
			t.Errorf("%s is not exclusively RunE", command.Name())
		}
		if err := command.Args(command, []string{"extra"}); err == nil {
			t.Errorf("%s accepted a positional argument", command.Name())
		}
	}
}

func TestVerifyMissingWorkspaceReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := verifyCommand(); err == nil {
		t.Fatal("verify returned success without a workspace")
	}
}

func TestDaemonCommandsReturnErrorsThroughCobra(t *testing.T) {
	commands := []*cobra.Command{
		daemonStartCmd, daemonStopCmd, daemonStatusCmd, daemonLogsCmd,
		daemonGatewayCmd, daemonRestartCmd, daemonMonitorCmd,
	}
	for _, command := range commands {
		if command.RunE == nil || command.Run != nil {
			t.Errorf("daemon %s is not exclusively RunE", command.Name())
		}
	}
}
