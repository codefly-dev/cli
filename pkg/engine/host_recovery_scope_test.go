package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/resources"
)

func recoveryTestWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, resources.WorkspaceConfigurationName), []byte("name: recovery-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHostAutomaticRecoveryRequiresKnownWorkspace(t *testing.T) {
	workspace := recoveryTestWorkspace(t)
	nested := filepath.Join(workspace, "source")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(nested, alias); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(workspace, "not-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	original := reapStaleProcessGroups
	t.Cleanup(func() { reapStaleProcessGroups = original })
	for _, test := range []struct {
		name, root              string
		wantRecovery, wantError bool
	}{
		{"workspace", workspace, true, false},
		{"nested source", nested, true, false},
		{"symlink source", alias, true, false},
		{"standalone source", t.TempDir(), false, false},
		{"missing below workspace", filepath.Join(workspace, "absent"), false, false},
		{"empty", "", false, true},
		{"file below workspace", file, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			reapStaleProcessGroups = func(_ context.Context, scope processgroup.Scope) (processgroup.CleanupEvidence, error) {
				calls++
				if scope.All() || !scope.Includes(workspace) || scope.Includes(t.TempDir()) {
					t.Fatalf("recovery escaped containing workspace: %+v", scope)
				}
				return processgroup.CleanupEvidence{}, nil
			}
			host, err := NewWorkspaceHost(Config{Root: test.root})
			if (err != nil) != test.wantError {
				t.Fatalf("construction error = %v", err)
			}
			if host != nil {
				if err := host.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if (calls == 1) != test.wantRecovery || calls > 1 {
				t.Fatalf("recovery calls = %d; wanted recovery %v", calls, test.wantRecovery)
			}
		})
	}
}
