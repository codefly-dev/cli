package deployments

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/posture"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// violatingWorkload starts a development server for a workload holding credential
// material, and mounts a Secret: two refusals of the deployed posture.
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
          args: ["server", "-dev"]
          env:
            - name: STORE_ROOT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: secret-store
                  key: STORE_ROOT_TOKEN
      volumes:
        - name: credentials
          secret:
            secretName: store-credentials
`

func cellEnvironment() *environments.Environment {
	return &environments.Environment{
		Name:    "staging",
		Cluster: &environments.EnvironmentCluster{Kind: "gke", Context: "cell"},
	}
}

// A render that is inspected rather than applied is still a restricted render of
// a cell's manifests, so it reaches the same guard.
func TestRenderManagerHoldsItsOutputToTheDeployedPosture(t *testing.T) {
	manager := NewRenderManager(&resources.Workspace{Name: "acme"}, cellEnvironment())
	module := &resources.Module{Name: "shop"}
	service := &resources.Service{Name: "store"}
	err := manager.checkPosture(context.Background(), module, service, violatingWorkload)
	require.ErrorContains(t, err, "deployed render refuses service shop/store")
	require.ErrorContains(t, err, posture.RuleNonScratchMount)

	// The same path reaches the other rules: without the mount, the development
	// server of a workload holding credential material is what is refused.
	withoutMount := violatingWorkload[:strings.Index(violatingWorkload, "      volumes:")]
	require.ErrorContains(t,
		manager.checkPosture(context.Background(), module, service, withoutMount),
		posture.RuleInMemoryStateStore)

	local := &environments.Environment{
		Name:    "local",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d},
	}
	localManager := NewRenderManager(&resources.Workspace{Name: "acme"}, local)
	require.NoError(t, localManager.checkPosture(context.Background(), module, service, violatingWorkload),
		"a render for a local cluster is not a cell")
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
	service := &resources.Service{Name: "store"}
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
