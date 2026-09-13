package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
)

// newRecoveryWorkspace writes a workspace declaring `local` with the given
// naming scope, makes it the working directory, and isolates CODEFLY_HOME so
// the resolved ownership never depends on the developer's own home.
func newRecoveryWorkspace(t *testing.T, namingScope string) string {
	t.Helper()
	root := t.TempDir()
	declaration := `name: recovery
layout: modules
environments:
    - name: local
`
	if namingScope != "" {
		declaration += "      naming-scope: " + namingScope + "\n"
	}
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), declaration)
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	t.Chdir(root)
	return root
}

// TestGenerateProjectsTheIdentityARunResolves is the whole point of projecting
// here. `generate` builds its containers in the CLI process, so nothing else
// ever labels them — and a label that is not the exact hash a run's sweep
// compares is collected by nothing, since these containers are not ephemeral
// and the cross-scope namespace sweep skips them. The identity must therefore
// be byte-identical to the one `codefly run` resolves for the same workspace.
func TestGenerateProjectsTheIdentityARunResolves(t *testing.T) {
	root := newRecoveryWorkspace(t, "from-yaml")

	got, err := containerRecoveryScope(context.Background())
	if err != nil {
		t.Fatalf("containerRecoveryScope: %v", err)
	}
	want, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "from-yaml")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if got != want {
		t.Fatalf("resolved scope = %+v, want the scope a run resolves %+v", got, want)
	}

	// The declared naming scope must actually reach the hash. Without it the
	// label would be well-formed, stable, and matched by no sweep the workspace
	// ever runs.
	unscoped, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if got == unscoped {
		t.Fatal("resolved scope ignores the workspace's declared naming scope")
	}
}

// TestGenerateProjectsTheMarkerIntoTheProcess covers the projection itself:
// core stamps the labels from the process marker, read back through
// InheritedContainerRecoveryScope, so an unprojected marker means unlabeled
// containers no matter what was resolved.
func TestGenerateProjectsTheMarkerIntoTheProcess(t *testing.T) {
	root := newRecoveryWorkspace(t, "from-yaml")

	projectContainerRecovery(context.Background())
	projected := dockerrun.InheritedContainerRecoveryScope()
	if projected == "" {
		t.Fatal("generate projected no container recovery marker")
	}

	want, err := dockerrun.NewContainerRecoveryScope(os.Getenv(resources.CodeflyHomeEnv), root, "from-yaml")
	if err != nil {
		t.Fatalf("NewContainerRecoveryScope: %v", err)
	}
	if err = dockerrun.SetContainerRecoveryScope(want); err != nil {
		t.Fatalf("SetContainerRecoveryScope: %v", err)
	}
	if identity := dockerrun.InheritedContainerRecoveryScope(); projected != identity {
		t.Fatalf("projected identity = %q, want %q", projected, identity)
	}
}

// TestGenerateOutsideAWorkspaceProjectsNothing holds the degradation. Ownership
// is a hash of a workspace that is not there, and substituting one would stamp
// containers with a durable identity no sweep can match — strictly worse than
// no label. Generating outside a workspace must still work.
func TestGenerateOutsideAWorkspaceProjectsNothing(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(dockerrun.ContainerRecoveryScopeEnvironment, "")
	t.Chdir(t.TempDir())

	if _, err := containerRecoveryScope(context.Background()); err == nil {
		t.Fatal("containerRecoveryScope resolved an ownership outside any workspace")
	}
	projectContainerRecovery(context.Background())
	if identity := dockerrun.InheritedContainerRecoveryScope(); identity != "" {
		t.Fatalf("projected %q outside a workspace, want no marker", identity)
	}
}
