package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func TestWithinKeepsOnlyTheClosure(t *testing.T) {
	values := map[string]int{"m/a": 1, "m/b": 2, "other/c": 3}
	require.Equal(t, map[string]int{"m/a": 1, "other/c": 3}, within(values, []string{"m/a", "other/c", "m/missing"}))
	require.Empty(t, within(values, nil))
}

// A module's render finds its services' trees where it would have found them
// had it deployed them itself, and reads what the deploy learned about the
// services of its closure — and nothing about the ones outside it.
func TestStageIntoCopiesTheModulesServicesAndReadsItsShare(t *testing.T) {
	scratch := t.TempDir()
	stage := t.TempDir()
	service := filepath.Join(scratch, "records", "services", "api", "kustomize")
	require.NoError(t, os.MkdirAll(service, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(service, "deployment.yaml"), []byte("kind: Deployment\n"), 0o644))
	// A service of another module that this module's closure reaches is not
	// staged beside the module's own; only its evidence is read.
	foreign := filepath.Join(scratch, "saas", "services", "accounts")
	require.NoError(t, os.MkdirAll(foreign, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(foreign, "deployment.yaml"), []byte("kind: Foreign\n"), 0o644))

	scratchRoot, err := newRenderRoot(scratch)
	require.NoError(t, err)
	defer scratchRoot.Close()
	tree, err := newRenderRoot(stage)
	require.NoError(t, err)
	defer tree.Close()

	output := &builderv0.DeploymentOutput{}
	deploy := &CompositionDeploy{scratch: scratch, scratchRoot: scratchRoot, modules: map[string]*moduleDeploy{
		"records": {
			members:        []string{"api", "gone"},
			outputs:        map[string]*builderv0.DeploymentOutput{"records/api": output},
			inClusterPorts: map[string]map[string]uint32{"records/api": {"grpc": 9000}},
		},
	}}
	rendered := &renderedRoots{
		outputs:          map[string]*builderv0.DeploymentOutput{},
		selfEndpoints:    map[string]map[string]string{},
		hostRoutes:       map[string]map[string]string{},
		endpointCarriers: map[string]string{},
		deployed:         map[string]*basev0.Configuration{},
		secretKeys:       map[string][]string{},
		inClusterPorts:   map[string]map[string]uint32{},
	}
	require.NoError(t, deploy.stageInto("records", stage, tree, rendered))

	copied, err := os.ReadFile(filepath.Join(stage, "services", "api", "kustomize", "deployment.yaml"))
	require.NoError(t, err)
	require.Equal(t, "kind: Deployment\n", string(copied))
	require.NoDirExists(t, filepath.Join(stage, "services", "accounts"))
	require.Same(t, output, rendered.outputs["records/api"])
	require.Equal(t, uint32(9000), rendered.inClusterPorts["records/api"]["grpc"])

	require.Error(t, deploy.stageInto("unknown", stage, tree, rendered), "a module the deploy never held must not render from nothing")

	// A module with no service to deploy is held, empty: it still renders its
	// bootstrap or bundle, and stages and reads nothing from the deploy.
	deploy.modules["bundle-only"] = &moduleDeploy{}
	before := len(rendered.outputs)
	require.NoError(t, deploy.stageInto("bundle-only", stage, tree, rendered))
	require.Len(t, rendered.outputs, before)
}

// The permissions an agent gave a file survive the move into a module's stage;
// the 0600 a bare write leaves would make every untouched file differ from what
// the module rendered in place.
func TestStageIntoKeepsTheModesTheAgentGave(t *testing.T) {
	scratch := t.TempDir()
	stage := t.TempDir()
	service := filepath.Join(scratch, "records", "services", "api")
	require.NoError(t, os.MkdirAll(service, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(service, "readable.yaml"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(service, "private.yaml"), []byte("b"), 0o600))
	scratchRoot, err := newRenderRoot(scratch)
	require.NoError(t, err)
	defer scratchRoot.Close()
	tree, err := newRenderRoot(stage)
	require.NoError(t, err)
	defer tree.Close()
	deploy := &CompositionDeploy{scratch: scratch, scratchRoot: scratchRoot, modules: map[string]*moduleDeploy{
		"records": {members: []string{"api"}},
	}}
	rendered := &renderedRoots{
		outputs: map[string]*builderv0.DeploymentOutput{}, selfEndpoints: map[string]map[string]string{},
		hostRoutes: map[string]map[string]string{}, endpointCarriers: map[string]string{},
		deployed: map[string]*basev0.Configuration{}, secretKeys: map[string][]string{}, inClusterPorts: map[string]map[string]uint32{},
	}
	require.NoError(t, deploy.stageInto("records", stage, tree, rendered))
	for name, want := range map[string]os.FileMode{"readable.yaml": 0o644, "private.yaml": 0o600} {
		info, statErr := os.Stat(filepath.Join(stage, "services", "api", name))
		require.NoError(t, statErr)
		require.Equal(t, want, info.Mode().Perm(), name)
	}
}
