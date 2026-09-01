package run

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestSolutionCommandReturnsErrors(t *testing.T) {
	if SolutionCmd.RunE == nil || SolutionCmd.Run != nil {
		t.Fatal("run solution must return errors through RunE")
	}
	if err := SolutionCmd.Args(SolutionCmd, []string{"unexpected"}); err == nil {
		t.Fatal("run solution accepted a positional argument")
	}
}

func TestResolveSolutionEntry(t *testing.T) {
	entry, err := resolveSolutionEntryFromDir(t, "testdata/solution")
	if err != nil {
		t.Fatalf("resolve solution entry: %v", err)
	}
	if entry != "wiki/backend" {
		t.Fatalf("solution entry = %q, want wiki/backend", entry)
	}
}

func TestResolveSolutionEntryNoRoot(t *testing.T) {
	_, err := resolveSolutionEntryFromDir(t, "testdata/solution-none")
	if err == nil {
		t.Fatal("expected an error when no module declares a service-entry")
	}
	if !strings.Contains(err.Error(), "no module declares a service-entry") {
		t.Fatalf("no-root error = %q", err)
	}
}

func TestResolveSolutionEntryAmbiguous(t *testing.T) {
	_, err := resolveSolutionEntryFromDir(t, "testdata/solution-ambiguous")
	if err == nil {
		t.Fatal("expected an error when several modules declare a service-entry")
	}
	if !strings.Contains(err.Error(), "ambiguous solution root") ||
		!strings.Contains(err.Error(), "wiki/backend") ||
		!strings.Contains(err.Error(), "host/gateway") {
		t.Fatalf("ambiguous error = %q", err)
	}
}

func resolveSolutionEntryFromDir(t *testing.T, dir string) (string, error) {
	t.Helper()
	ctx := t.Context()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	if err != nil {
		t.Fatalf("load workspace %s: %v", dir, err)
	}
	return resolveSolutionEntry(ctx, workspace)
}
