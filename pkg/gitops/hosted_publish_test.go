package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// hostedPaymentsInstances is the presence the hosted fixture declares.
func hostedPaymentsInstances() []SolutionInstance {
	return []SolutionInstance{{
		Kind: solutionhost.KindModule, Name: "payments", Package: "example/payments", Version: "1.0.0", ReleaseDigest: testReleaseDigest,
		Units: []SolutionArtifactUnit{
			{Name: "api", Path: "services/api", Subject: "payments@example.iam.test"},
			{Name: "host", Path: "services/host", Subject: "payments@example.iam.test"},
		},
	}}
}

// renderHostedPayments renders the payments module of the hosted fixture with
// the given Deployment manifest for its two services and the presence it
// declares; nil declares none, which withdraws it.
func renderHostedPayments(t *testing.T, workspace *resources.Workspace, env *environments.Environment, deployment string, instances []SolutionInstance) {
	t.Helper()
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", "payments")
	if _, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: destination, Module: "payments", UnitNames: []string{"api", "host"},
		OwnedPath:   filepath.ToSlash(filepath.Join("environments", "deployments", "modules", "payments")),
		Units:       promotableServiceGraph("payments", []string{"api", "host"}),
		Environment: "production", Namespace: "payments", AppProject: "payments", Promotable: true,
		Workspace: "payments", Host: env.Host, Target: env,
		SolutionInstances: instances,
	}, func(_ context.Context, stage string) error {
		for _, name := range []string{"api", "host"} {
			overlay := filepath.Join(stage, "services", name, "overlays", "production")
			if err := os.MkdirAll(overlay, 0o755); err != nil {
				return err
			}
			manifest := strings.ReplaceAll(deployment, "name: api", "name: "+name)
			if err := os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(manifest), 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"), 0o644); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
