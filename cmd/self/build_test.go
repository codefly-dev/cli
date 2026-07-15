package self

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	if err := buildCLI(ctx, t.TempDir(), output); err == nil {
		t.Fatal("buildCLI unexpectedly succeeded with a cancelled context")
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "existing" {
		t.Fatalf("existing output changed after cancelled build: %q", contents)
	}
}
