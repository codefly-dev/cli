package run

import (
	"errors"
	"os"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestExecuteManagedCommandCarriesExplicitChildExitStatus(t *testing.T) {
	err := executeManagedCommand(t.Context(), []string{
		os.Args[0], "-test.run=^TestManagedCommandChildProcess$", "--", "exit-17",
	})
	var commandErr *managedCommandExitError
	if !errors.As(err, &commandErr) {
		t.Fatalf("executeManagedCommand() error = %T %v, want typed child exit", err, err)
	}
	if commandErr.CommandExitCode() != 17 {
		t.Fatalf("child exit status = %d, want 17", commandErr.CommandExitCode())
	}
}

func TestManagedCommandChildProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "exit-17" {
		return
	}
	if os.Getenv(resources.RunningPrefix) != "true" {
		os.Exit(16)
	}
	os.Exit(17)
}
