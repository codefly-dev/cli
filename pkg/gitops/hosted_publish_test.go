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

// hostedPaymentsInstances is the module instance the hosted fixture declares:
// its api runs as the module.
func hostedPaymentsInstances() *ModuleInstance {
	return &ModuleInstance{
		Kind: solutionhost.KindModule, Name: "payments", Package: "example/payments", Version: "1.0.0",
		Identities: []IdentityWorkload{{Service: "api", Path: "services/api"}},
		Authority:  &solutionhost.Authority{Principal: "payments"},
	}
}

// renderHostedPayments renders the payments module of the hosted fixture with
// the given Deployment manifest for its two services and the instance it
// declares; nil declares none, which withdraws it.
func renderHostedPayments(t *testing.T, workspace *resources.Workspace, env *environments.Environment, deployment string, instance *ModuleInstance) {
	t.Helper()
	destination := filepath.Join(workspace.Dir(), "deployments", "modules", "payments")
	// The owned path is the render's own rule, so the fixture follows the
	// workspace's gitops path exactly as a real render does.
	ownedPath, err := moduleOwnedPath(workspace, env, "payments")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: destination, Module: "payments", UnitNames: []string{"api", "host"},
		OwnedPath:   ownedPath,
		Units:       promotableServiceGraph("payments", []string{"api", "host"}),
		Environment: "production", Namespace: "payments", AppProject: "payments", Promotable: true,
		Workspace: "payments", Host: env.Host, Target: env,
		Instance: instance,
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
