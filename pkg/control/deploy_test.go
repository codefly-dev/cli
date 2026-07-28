package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/stretchr/testify/require"
)

func TestRunDeployRejectsRemoteTargetBeforeStartingFlow(t *testing.T) {
	root := writeWorkspace(t)
	workspace := fixtureWorkspaceYAML + `environments:
    - name: production
      cluster:
          kind: eks
          kubeconfig: /does/not/exist
          context: k3d-production
`
	if err := os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(workspace), 0o600); err != nil {
		t.Fatal(err)
	}
	plane, err := NewAt(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	_, err = plane.Deploy(context.Background(), DeployRequest{
		Service: "backend/api",
		Env:     "production",
	})

	if err == nil {
		t.Fatal("remote direct deploy succeeded")
	}
	if got := err.Error(); !containsAll(got, "exact local k3d target", "--render-only", "GitOps") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeployResultIncludesEveryRenderedTreeAndExactTarget(t *testing.T) {
	provider := staticEvidenceProvider{evidence: deployments.DeploymentEvidence{
		Target: &deployments.VerifiedKubernetesTarget{
			Kind:            "k3d",
			Kubeconfig:      "/tmp/kubeconfig",
			Context:         "k3d-dev",
			Cluster:         "k3d-dev",
			APIServer:       "https://127.0.0.1:6443",
			K3dCluster:      "dev",
			ClusterIdentity: "sha256:cluster",
		},
		RenderedTrees: []deployments.RenderedTreeEvidence{
			{Module: "backend", Service: "api", Digest: "sha256:api", Manifests: "kind: Deployment\n"},
			{Module: "shared", Service: "database", Digest: "sha256:database", Manifests: "kind: StatefulSet\n"},
		},
	}}

	result, err := deployResult(true, provider)

	require.NoError(t, err)
	require.True(t, result.Succeeded)
	require.Equal(t, "sha256:cluster", result.Target.ClusterIdentity)
	require.Equal(t, []RenderedTree{
		{Module: "backend", Service: "api", Digest: "sha256:api", Manifests: "kind: Deployment\n"},
		{Module: "shared", Service: "database", Digest: "sha256:database", Manifests: "kind: StatefulSet\n"},
	}, result.RenderedTrees)
}

func TestDeployResultCarriesRenderedManifestsWithoutMutationTarget(t *testing.T) {
	provider := staticEvidenceProvider{evidence: deployments.DeploymentEvidence{
		RenderedTrees: []deployments.RenderedTreeEvidence{{
			Module:    "backend",
			Service:   "api",
			Digest:    "sha256:api",
			Manifests: "kind: Deployment\n",
		}},
	}}

	result, err := deployResult(true, provider)

	require.NoError(t, err)
	require.Nil(t, result.Target)
	require.Equal(t, "kind: Deployment\n", result.RenderedTrees[0].Manifests)
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

type staticEvidenceProvider struct {
	evidence deployments.DeploymentEvidence
}

func (p staticEvidenceProvider) Evidence() deployments.DeploymentEvidence {
	return p.evidence
}
