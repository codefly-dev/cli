package gitops

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestCopyEnvironmentBootstrapPreservesSharedBaseAndExcludesOtherEnvironments(t *testing.T) {
	source := t.TempDir()
	base := filepath.Join(source, "base")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(base, "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "deployment.yaml"), []byte(pinnedDeployment), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"local", "aws"} {
		root := filepath.Join(source, "overlays", environment)
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(root, "kustomization.yaml"),
			[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "bootstrap")
	if err := copyEnvironmentBootstrap(source, "local", destination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "base", "deployment.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "overlays", "local", "kustomization.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destination, "overlays", "aws")); !os.IsNotExist(err) {
		t.Fatalf("unselected environment copied: %v", err)
	}
	if _, err := validateTree(destination, &RenderOptions{Promotable: true}); err != nil {
		t.Fatalf("selected bootstrap dependency graph is invalid: %v", err)
	}
}

func TestModuleRenderRootsCoverDependencyGraph(t *testing.T) {
	services := []*resources.Service{
		{Name: "frontend", ServiceDependencies: []*resources.ServiceDependency{{Name: "accounts"}}},
		{Name: "accounts", ServiceDependencies: []*resources.ServiceDependency{{Name: "store"}, {Name: "vault"}}},
		{Name: "forge-edge", ServiceDependencies: []*resources.ServiceDependency{{Name: "store"}, {Name: "vault"}}},
		{Name: "store"},
		{Name: "vault"},
	}

	roots, err := moduleRenderRoots("users", services)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, root := range roots {
		names = append(names, root.Name)
	}
	if got, want := names, []string{"frontend", "forge-edge"}; !slices.Equal(got, want) {
		t.Fatalf("roots = %v, want %v", got, want)
	}
}

func TestModuleRenderRootsRejectCycle(t *testing.T) {
	services := []*resources.Service{
		{Name: "one", ServiceDependencies: []*resources.ServiceDependency{{Name: "two"}}},
		{Name: "two", ServiceDependencies: []*resources.ServiceDependency{{Name: "one"}}},
	}

	if _, err := moduleRenderRoots("users", services); err == nil {
		t.Fatal("cyclic module graph was accepted")
	}
}

// Before #474, snapshot push (and this registry preparation) was gated on
// cluster.kind == "k3d", so every managed render dead-ended with "snapshot
// build requires push". Registry preparation is now cluster-agnostic: any
// environment declaring a registry URL prepares cleanly whatever the cluster
// kind, including a nil cluster. Auth is empty here (managed registries
// authenticate out-of-band), so no credential helper is invoked.
func TestPrepareSnapshotRegistryIgnoresClusterKind(t *testing.T) {
	registry := &resources.EnvironmentRegistry{URL: "registry.example.com/team"}
	for _, cluster := range []*resources.EnvironmentCluster{
		{Kind: "k3d"},
		{Kind: "eks"},
		{Kind: "aks"},
		{Kind: ""},
		nil,
	} {
		env := &resources.Environment{Name: "snap", Cluster: cluster, Registry: registry}
		if err := prepareSnapshotRegistry(context.Background(), env); err != nil {
			t.Fatalf("cluster %+v: %v", cluster, err)
		}
	}
}

func TestPrepareSnapshotRegistryRequiresRegistryURL(t *testing.T) {
	for _, env := range []*resources.Environment{
		{Name: "snap"},
		{Name: "snap", Registry: &resources.EnvironmentRegistry{URL: "   "}},
	} {
		if err := prepareSnapshotRegistry(context.Background(), env); err == nil {
			t.Fatalf("registry %+v was accepted without a URL", env.Registry)
		}
	}
}
