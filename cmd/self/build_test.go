package self

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// The --with-agents path forwards -j/--jobs into BuildOptions.Jobs for parity
// with `agent build --all`, so the flag must exist with the same shorthand and
// a default of 0 (which BuildAllAgents interprets as runtime.NumCPU()).
func TestBuildCmdJobsFlag(t *testing.T) {
	f := BuildCmd.Flags().Lookup("jobs")
	if f == nil {
		t.Fatal("self build is missing the --jobs flag")
	}
	if f.Shorthand != "j" {
		t.Errorf("--jobs shorthand = %q, want \"j\"", f.Shorthand)
	}
	if f.DefValue != "0" {
		t.Errorf("--jobs default = %q, want \"0\" (NumCPU)", f.DefValue)
	}

	jobs, err := BuildCmd.Flags().GetInt("jobs")
	if err != nil {
		t.Fatalf("GetInt(jobs): %v", err)
	}
	if jobs != 0 {
		t.Errorf("default jobs = %d, want 0", jobs)
	}
}

func TestResolveAgentRootFindsFlatWorkspace(t *testing.T) {
	root := t.TempDir()
	cliDir := filepath.Join(root, "cli")
	agentDir := filepath.Join(root, "service-example")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.codefly.yaml"), []byte("name: example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveAgentRoot(cliDir)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("resolveAgentRoot() = %q, want %q", got, root)
	}
}

func TestResolveAgentRootRejectsWorkspaceWithoutAgents(t *testing.T) {
	cliDir := filepath.Join(t.TempDir(), "cli")
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAgentRoot(cliDir); err == nil {
		t.Fatal("resolveAgentRoot unexpectedly accepted a workspace without agents")
	}
}

func TestResolveAgentRootDoesNotEscapeImmediateWorkspace(t *testing.T) {
	outer := t.TempDir()
	unrelatedAgent := filepath.Join(outer, "service-unrelated")
	cliDir := filepath.Join(outer, "nested", "workspace", "cli")
	if err := os.MkdirAll(unrelatedAgent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelatedAgent, "agent.codefly.yaml"), []byte("name: unrelated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if got, err := resolveAgentRoot(cliDir); err == nil {
		t.Fatalf("resolveAgentRoot escaped the immediate workspace and selected %q", got)
	}
}

func TestReportSkippedPluginCheckouts(t *testing.T) {
	skipped := []skippedPluginCheckout{
		{name: "module-saas-starter-onboarding-library", originRepository: "module-saas-starter"},
		{name: "service-postgres-docker-retry", originRepository: "service-postgres"},
	}
	var got []string
	cli.SetOutputSink(func(_ wool.Loglevel, message string) {
		got = append(got, message)
	})
	t.Cleanup(func() {
		cli.SetOutputSink(nil)
	})

	reportSkippedPluginCheckouts(skipped)

	want := []string{
		"Skipping 2 duplicate or task-specific plugin checkout(s):",
		"  - module-saas-starter-onboarding-library (origin repository: module-saas-starter)",
		"  - service-postgres-docker-retry (origin repository: service-postgres)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skip output = %q, want %q", got, want)
	}
}

func TestSelfCommandsReturnErrors(t *testing.T) {
	for name, command := range map[string]*cobra.Command{
		"build": BuildCmd,
		"pull":  PullCmd,
	} {
		if command.RunE == nil || command.Run != nil {
			t.Fatalf("self %s must return errors through RunE", name)
		}
		if err := command.Args(command, []string{"extra"}); err == nil {
			t.Fatalf("self %s accepted positional arguments", name)
		}
	}
}

func TestBuildCLICancellationPreservesExistingOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "bin", "codefly")
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := buildCLI(ctx, t.TempDir(), output); !errors.Is(err, context.Canceled) {
		t.Fatalf("buildCLI cancellation error = %v, want context.Canceled", err)
	} else if strings.Count(err.Error(), context.Canceled.Error()) != 1 {
		t.Fatalf("buildCLI cancellation error = %q, want one cancellation", err)
	}
	if contents, err := os.ReadFile(output); err != nil {
		t.Fatal(err)
	} else if string(contents) != "existing" {
		t.Fatalf("existing output changed after cancelled build: %q", contents)
	}
}

func TestBuildCLICrossReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := buildCLICross(ctx, t.TempDir(), filepath.Join(t.TempDir(), "codefly"), "linux", "amd64")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("buildCLICross cancellation error = %v, want context.Canceled", err)
	}
	if strings.Count(err.Error(), context.Canceled.Error()) != 1 {
		t.Fatalf("buildCLICross cancellation error = %q, want one cancellation", err)
	}
}
