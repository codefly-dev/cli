package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	applied := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
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
		Required: deployments.StageApplied,
		Reached:  deployments.StageApplied,
		RenderedTrees: []deployments.RenderedTreeEvidence{
			{
				Module: "backend", Service: "api", Digest: "sha256:api", Manifests: "kind: Deployment\n",
				Stage: deployments.StageApplied, AppliedAt: applied,
			},
			{
				Module: "shared", Service: "database", Digest: "sha256:database", Manifests: "kind: StatefulSet\n",
				Stage: deployments.StageApplied, AppliedAt: applied,
			},
		},
	}}

	result, err := deployResult(true, provider)

	require.NoError(t, err)
	require.True(t, result.Succeeded)
	require.Equal(t, "sha256:cluster", result.Target.ClusterIdentity)
	require.Equal(t, deployments.StageApplied, result.Required)
	require.Equal(t, deployments.StageApplied, result.Reached)
	require.Equal(t, []RenderedTree{
		{
			Module: "backend", Service: "api", Digest: "sha256:api", Manifests: "kind: Deployment\n",
			Stage: deployments.StageApplied, AppliedAt: applied,
		},
		{
			Module: "shared", Service: "database", Digest: "sha256:database", Manifests: "kind: StatefulSet\n",
			Stage: deployments.StageApplied, AppliedAt: applied,
		},
	}, result.RenderedTrees)
}

func TestDeployResultCarriesRenderedManifestsWithoutMutationTarget(t *testing.T) {
	provider := staticEvidenceProvider{evidence: deployments.DeploymentEvidence{
		Required: deployments.StageRendered,
		Reached:  deployments.StageRendered,
		RenderedTrees: []deployments.RenderedTreeEvidence{{
			Module:    "backend",
			Service:   "api",
			Digest:    "sha256:api",
			Manifests: "kind: Deployment\n",
			Stage:     deployments.StageRendered,
		}},
	}}

	result, err := deployResult(true, provider)

	require.NoError(t, err)
	require.Nil(t, result.Target)
	require.Equal(t, "kind: Deployment\n", result.RenderedTrees[0].Manifests)
}

// A caller that required bootstrapping cannot be handed a success just because
// kubectl apply returned zero: apply establishes applied and nothing more.
func TestDeployResultRefusesSuccessShortOfTheRequiredStage(t *testing.T) {
	provider := staticEvidenceProvider{evidence: deployments.DeploymentEvidence{
		Required: deployments.StageBootstrapped,
		Reached:  deployments.StageApplied,
		RenderedTrees: []deployments.RenderedTreeEvidence{{
			Module:      "backend",
			Service:     "api",
			Digest:      "sha256:api",
			Manifests:   "kind: Deployment\n",
			Stage:       deployments.StageApplied,
			Diagnostics: []string{"Job backend/schema-migrate failed: BackoffLimitExceeded"},
		}},
	}}

	result, err := deployResult(true, provider)

	require.ErrorContains(t, err, "reached applied, short of the required bootstrapped")
	require.False(t, result.Succeeded)
	require.Equal(t, []string{"Job backend/schema-migrate failed: BackoffLimitExceeded"}, result.RenderedTrees[0].Diagnostics)
}

// A dry run contacts no cluster, so a caller asking it for health must be
// refused rather than handed a green result for a stage nothing verified.
func TestDryRunRefusesACompletionItCannotEstablish(t *testing.T) {
	plane, err := NewAt(writeWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	_, err = plane.Deploy(context.Background(), DeployRequest{
		Service:    "backend/api",
		DryRun:     true,
		Completion: deployments.StageHealthy,
	})

	require.ErrorContains(t, err, "a dry run cannot establish healthy")
}

func TestDeployCompletionKeepsAppliedWhenTheCallerNamesNoStage(t *testing.T) {
	require.Equal(t, deployments.StageApplied, deployCompletion(&DeployRequest{}).Stage)
	require.Equal(t, deployments.DefaultCompletionTimeout, deployCompletion(&DeployRequest{}).Timeout)

	requested := deployCompletion(&DeployRequest{
		Completion:        deployments.StageHealthy,
		CompletionTimeout: 90 * time.Second,
	})
	require.Equal(t, deployments.StageHealthy, requested.Stage)
	require.Equal(t, 90*time.Second, requested.Timeout)
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
