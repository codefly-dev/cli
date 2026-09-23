package run

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func loadSolutionFixture(t *testing.T, name string) *resources.Workspace {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("cannot load workspace %s: %v", name, err)
	}
	return workspace
}

// A product composition composes a host beside a solution, both by source and
// version, and both declare a service-entry. The solution's entry depends on
// the host, which makes the host a dependency and the solution the root — so
// `run solution` needs no name to boot the solution, and never boots the host's
// own entry instead.
func TestResolveSolutionEntryPrefersTheEntryNothingElseDependsOn(t *testing.T) {
	workspace := loadSolutionFixture(t, "solution-composed")
	entry, err := ResolveSolutionEntry(context.Background(), workspace)
	if err != nil {
		t.Fatalf("ResolveSolutionEntry: %v", err)
	}
	if entry != "app/backend" {
		t.Fatalf("entry = %q, want the solution's app/backend, not the host's entry", entry)
	}
}

func TestResolveSolutionEntryFindsTheOnlyEntry(t *testing.T) {
	workspace := loadSolutionFixture(t, "solution")
	entry, err := ResolveSolutionEntry(context.Background(), workspace)
	if err != nil {
		t.Fatalf("ResolveSolutionEntry: %v", err)
	}
	if entry != "wiki/backend" {
		t.Fatalf("entry = %q, want wiki/backend", entry)
	}
}

// Two entries with no dependency between them are genuinely ambiguous — unless
// one is the workspace's own module, which is what the workspace is for.
func TestResolveSolutionEntryBreaksATieOnTheWorkspacesOwnModule(t *testing.T) {
	workspace := loadSolutionFixture(t, "solution-ambiguous")
	_, err := ResolveSolutionEntry(context.Background(), workspace)
	if err == nil || !strings.Contains(err.Error(), "ambiguous solution root") {
		t.Fatalf("expected an ambiguous-root error, got %v", err)
	}
	for _, unique := range []string{"wiki/backend", "host/gateway"} {
		if !strings.Contains(err.Error(), unique) {
			t.Errorf("error %q does not name the competing root %s", err, unique)
		}
	}

	workspace.Name = "wiki"
	entry, err := ResolveSolutionEntry(context.Background(), workspace)
	if err != nil {
		t.Fatalf("ResolveSolutionEntry with the workspace named like one root: %v", err)
	}
	if entry != "wiki/backend" {
		t.Fatalf("entry = %q, want the workspace's own module to break the tie", entry)
	}
}

func TestResolveSolutionEntryReportsAWorkspaceWithNoEntry(t *testing.T) {
	workspace := loadSolutionFixture(t, "solution-none")
	_, err := ResolveSolutionEntry(context.Background(), workspace)
	if err == nil || !strings.Contains(err.Error(), "no module declares a service-entry") {
		t.Fatalf("expected a no-entry error, got %v", err)
	}
}
