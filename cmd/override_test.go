package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func findCommand(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	command := RootCmd
	for _, name := range path {
		var next *cobra.Command
		for _, child := range command.Commands() {
			if child.Name() == name {
				next = child
				break
			}
		}
		if next == nil {
			t.Fatalf("%s registers no subcommand %q", command.CommandPath(), name)
		}
		command = next
	}
	return command
}

func TestOverrideServiceIsRegistered(t *testing.T) {
	command := findCommand(t, "override", "service")
	if command.RunE == nil {
		t.Fatal("override service has no RunE")
	}
}

// Help is the contract: `codefly explain` reprints this static text, and it has
// to be complete without network access.
func TestOverrideServiceHelpStandsAlone(t *testing.T) {
	command := findCommand(t, "override", "service")
	var output bytes.Buffer
	command.SetOut(&output)
	t.Cleanup(func() { command.SetOut(nil) })
	if err := command.Help(); err != nil {
		t.Fatalf("help: %v", err)
	}
	rendered := output.String()
	for _, expected := range []string{
		"codefly.local.yaml", "--path", "--worktree", "--version", "--clear",
		"gitignored", "--service-path",
	} {
		if !strings.Contains(rendered, expected) {
			t.Errorf("help does not mention %q", expected)
		}
	}
}

func TestOverrideRejectsUnknownSubcommand(t *testing.T) {
	configureSubcommandValidation(RootCmd)
	command := findCommand(t, "override")
	err := command.Args(command, []string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("unknown subcommand was not rejected: %v", err)
	}
}
