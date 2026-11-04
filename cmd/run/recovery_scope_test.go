package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/processgroup"
)

func TestRunCommandRecoveryWaitsForResolvedWorkspace(t *testing.T) {
	fixture, _, _ := loadRecoveryFixture(t, "")
	workspace := fixture.Dir()
	foreign := t.TempDir()
	// Invoke from the service directory: recovery must still select its workspace.
	t.Chdir(filepath.Join(workspace, "modules/app/services/api"))
	previousReap, previousPins, previousHeadless := reapRunProcesses, pinsAlreadyResolved, headless
	previousScopeExplicit := namingScopeExplicit
	t.Cleanup(func() {
		reapRunProcesses, pinsAlreadyResolved, headless = previousReap, previousPins, previousHeadless
		namingScopeExplicit = previousScopeExplicit
	})
	t.Setenv(processgroup.LaunchWorkspaceEnv, foreign)
	pinsAlreadyResolved, headless = true, true
	for _, test := range []struct {
		name string
		args []string
		want int
	}{
		{"failure before resolution", []string{"missing/api"}, 0},
		{"failure after resolution", []string{"app/api", "missing/api"}, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			reapRunProcesses = func(_ context.Context, scope processgroup.Scope) (processgroup.CleanupEvidence, error) {
				calls++
				if scope.All() || !scope.Includes(workspace) || scope.Includes(foreign) {
					t.Fatalf("command recovery scope escaped workspace: %+v", scope)
				}
				if got := os.Getenv(processgroup.LaunchWorkspaceEnv); got != workspace {
					t.Fatalf("resolved launch attribution = %q; want %q", got, workspace)
				}
				return processgroup.CleanupEvidence{}, nil
			}
			if err := runServiceCommand(ServiceCmd, test.args); err == nil {
				t.Fatal("expected missing service to refuse before graph startup")
			}
			if calls != test.want {
				t.Fatalf("recovery calls = %d; want %d", calls, test.want)
			}
		})
	}
}
