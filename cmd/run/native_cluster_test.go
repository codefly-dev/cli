package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/nativecluster"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeNonGoWorkload lays down a workspace whose one module runs its service
// on an agent that is not the Go family, and returns the loaded service.
func writeNonGoWorkload(t *testing.T) (*resources.Workspace, *resources.Service) {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                 "name: acme\nlayout: modules\nmodules:\n  - name: model\n",
		filepath.Join("modules", "model", resources.ModuleConfigurationName): "kind: module\nname: model\nservices:\n  - name: model\n",
		filepath.Join("modules", "model", "services", "model", resources.ServiceConfigurationName): `kind: service
name: model
version: 0.0.0
module-identity: true
agent:
  kind: runtime::service
  name: python-fastapi
  version: 0.0.112
  publisher: codefly.dev
`,
	}
	for relative, content := range files {
		full := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	service, err := workspace.LoadService(ctx, &resources.ServiceWithModule{Name: "model", Module: "model"})
	require.NoError(t, err)
	return workspace, service
}

// A delivery reads the build from the agent's report, so a workload whose
// agent has not adopted the contract is refused by name — and the refusal
// arrives before the run touches its authority, its gateway or the host's
// envelope. This is the shape of #989: the old path joined
// <service>/.cache/native/main for every agent and failed on a missing file
// that a python service was never going to have.
func TestANativeDeliveryRefusesAWorkloadWhoseAgentReportsNoArtifact(t *testing.T) {
	workspace, service := writeNonGoWorkload(t)
	unique := resources.WithUnique(service).Unique()
	cluster := &nativeCluster{
		workspace: workspace,
		workloads: map[string]nativecluster.Workload{unique: {Namespace: "acme-model", Account: "model"}},
		// A flow holding no manager for the service answers as an agent that
		// reported nothing: there is no started artifact to be had.
		flow: &orchestration.Flow{},
	}
	peer := gitops.NativePeer{Module: "model", Service: "model", Account: "model", Consumer: true}

	err := cluster.deliver(context.Background(), unique, peer, service)
	require.ErrorContains(t, err, "codefly.dev/python-fastapi:0.0.112", "the refusal must name the agent that has to adopt the contract")
	require.ErrorContains(t, err, "runtime-started-artifact/v1")
	require.ErrorContains(t, err, "model/model")
	require.NotContains(t, err.Error(), ".cache", "the refusal must not name a path from one agent family's layout")
	require.NotContains(t, err.Error(), "no such file", "a missing file is not the diagnosis: the agent reported nothing")

	// Refused before anything is claimed on the workload's behalf: no
	// documents, no approved build, nothing for the host to reconcile.
	require.Empty(t, cluster.documents)
	require.Empty(t, cluster.builds)
}

// A workload the run never projected an identity for is a different failure
// from one whose agent reported nothing, and must read as one.
func TestANativeDeliveryNamesAnUnprojectedWorkloadAsSuch(t *testing.T) {
	workspace, service := writeNonGoWorkload(t)
	cluster := &nativeCluster{workspace: workspace, workloads: map[string]nativecluster.Workload{}}
	err := cluster.deliver(context.Background(), resources.WithUnique(service).Unique(),
		gitops.NativePeer{Module: "model", Service: "model", Consumer: true}, service)
	require.ErrorContains(t, err, "no projected identity")
}
