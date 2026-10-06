package deployments

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/posture"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// violatingWorkload mounts a Secret, which is not the standard scratch volume.
const violatingWorkload = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: store
  namespace: acme
spec:
  selector:
    matchLabels:
      app: store
  template:
    metadata:
      labels:
        app: store
    spec:
      containers:
        - name: store
          image: registry.example.com/acme/store@sha256:aaaa
      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`

// certificateMaterial is a ConfigMap the render would deliver, carrying material
// under Kubernetes' own conventional key.
const certificateMaterial = `apiVersion: v1
kind: ConfigMap
metadata:
  name: store-trust
  namespace: acme
data:
  ca.crt: material
`

func cellEnvironment() *environments.Environment {
	return &environments.Environment{
		Name:    "staging",
		Cluster: &environments.EnvironmentCluster{Kind: "managed", Context: "cell"},
	}
}

// A render that is inspected rather than applied is still a restricted render of
// a cell's manifests, so it reaches the same guard.
func TestRenderManagerHoldsItsOutputToTheDeployedPosture(t *testing.T) {
	manager := NewRenderManager(&resources.Workspace{Name: "acme"}, cellEnvironment())
	module := &resources.Module{Name: "shop"}
	service := &resources.Service{Name: "store", Spec: map[string]any{
		"deployment": map[string]any{"storage": "durable", "transport": "mesh"},
	}}
	err := manager.checkPosture(context.Background(), module, service, violatingWorkload)
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)

	// The same path reaches the other rules: certificate material the render
	// delivers is refused on a mesh-protected environment.
	meshed := cellEnvironment()
	meshed.Posture = &posture.Declaration{
		Asserts: map[string]bool{posture.AssertMeshProtectedTransport: true},
	}
	meshedManager := NewRenderManager(&resources.Workspace{Name: "acme"}, meshed)
	require.ErrorContains(t,
		meshedManager.checkPosture(context.Background(), module, service, certificateMaterial),
		posture.RulePeerTransportMaterial)

	local := &environments.Environment{
		Name:    "local",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d},
	}
	localManager := NewRenderManager(&resources.Workspace{Name: "acme"}, local)
	require.NoError(t, localManager.checkPosture(context.Background(), module, service, violatingWorkload),
		"a render for a local cluster is not a cell")
}

// A dry run prints the exceptions it relies on, like every other deployed render.
func TestRenderManagerReportsTheEnvironmentsAllowances(t *testing.T) {
	env := cellEnvironment()
	env.Posture = &posture.Declaration{Allowances: []posture.Allowance{{
		Rule: posture.RuleNonScratchMount, Service: "shop/store", Reason: "a durable data claim, reviewed",
	}}}
	manager := NewRenderManager(&resources.Workspace{Name: "acme"}, env)
	require.Equal(t, []string{
		"security posture: service shop/store is allowed to break rule non-scratch-mount — a durable data claim, reviewed",
	}, manager.PostureAllowances())

	local := &environments.Environment{
		Name:    "local",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d},
		Posture: env.Posture,
	}
	require.Nil(t, NewRenderManager(&resources.Workspace{Name: "acme"}, local).PostureAllowances(),
		"a render that is not for a cell is not held to the posture and reports no exception to it")
}

// The dry-run path end to end: the manager builds the tree with Kustomize and
// must refuse what it built. It needs the kustomize binary, as that path always
// has.
func TestRenderManagerHandleRefusesAViolatingDryRun(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize is not installed; the dry-run path shells out to it")
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName),
		[]byte("name: acme\nlayout: modules\n"), 0o644))
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	module := &resources.Module{Name: "shop"}
	service := &resources.Service{Name: "store", Spec: map[string]any{
		"deployment": map[string]any{"storage": "durable", "transport": "mesh"},
	}}
	env := cellEnvironment()
	base := filepath.Join(root, "deployments", "modules", "shop", "services", "store", "base")
	overlay := filepath.Join(root, "deployments", "modules", "shop", "services", "store", "overlays", "staging")
	for _, directory := range []string{base, overlay} {
		require.NoError(t, os.MkdirAll(directory, 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(base, "workload.yaml"), []byte(violatingWorkload), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "kustomization.yaml"),
		[]byte("resources:\n  - workload.yaml\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "kustomization.yaml"),
		[]byte("resources:\n  - ../../base\n"), 0o644))

	manager := NewRenderManager(workspace, env)
	output := &builderv0.DeploymentOutput{Kind: &builderv0.DeploymentOutput_Kubernetes{
		Kubernetes: &builderv0.KubernetesDeploymentOutput{
			Kind:    builderv0.KubernetesDeploymentOutput_KUSTOMIZE,
			Profile: builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
		},
	}}
	require.ErrorContains(t, manager.Handle(context.Background(), service, module, output),
		"deployed render refuses service shop/store")
}
