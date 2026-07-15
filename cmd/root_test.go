package cmd

import (
	"testing"

	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

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
