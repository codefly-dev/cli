package run

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	coresdk "github.com/codefly-dev/core/sdk"
)

func TestManagedCommandPreflightRejectsAmbiguousWorkspaceWithoutTerminalSelection(t *testing.T) {
	t.Chdir("../../pkg/orchestration/testdata/module-layout")

	err := preflightManagedCommandService(t.Context())
	if err == nil {
		t.Fatal("managed command accepted ambiguous workspace context")
	}
	for _, want := range []string{"multiple services found", "frontend", "gateway", "run from a service directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("managed command preflight error = %q, want %q", err, want)
		}
	}
}

func TestManagedCommandPinsDependencyRunnerToInvokingExecutable(t *testing.T) {
	option := &coresdk.Option{}
	for _, apply := range managedCommandDependencyOptions("/opt/codefly/releases/v0.1.104/codefly") {
		apply(option)
	}
	if got := option.CodeflyBinary; got != "/opt/codefly/releases/v0.1.104/codefly" {
		t.Fatalf("dependency runner = %q, want invoking executable", got)
	}
}

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
