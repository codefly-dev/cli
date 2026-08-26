package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

func TestWorkspaceStagedModulePathKeepsVendoredAndCanonicalizesReferenced(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	external := t.TempDir()

	vendored, err := resources.LoadWorkspaceFromDir(ctx, writeReferencedModuleWorkspace(t, root, filepath.Join(root, "modules", "vendored")))
	if err != nil {
		t.Fatal(err)
	}
	vendoredModule, err := vendored.LoadModuleFromName(ctx, "payments")
	if err != nil {
		t.Fatal(err)
	}
	if got := workspaceStagedModulePath(vendored, vendoredModule); got != filepath.Join("modules", "vendored") {
		t.Fatalf("vendored staged path = %q, want modules/vendored", got)
	}

	referenced, err := resources.LoadWorkspaceFromDir(ctx, writeReferencedModuleWorkspace(t, t.TempDir(), filepath.Join(external, "payments")))
	if err != nil {
		t.Fatal(err)
	}
	referencedModule, err := referenced.LoadModuleFromName(ctx, "payments")
	if err != nil {
		t.Fatal(err)
	}
	if got := workspaceStagedModulePath(referenced, referencedModule); got != filepath.Join("modules", "payments") {
		t.Fatalf("referenced staged path = %q, want modules/payments", got)
	}
}

// TestRenderModuleBundleStagesReferencedOutOfRepoModule proves a module declared
// by an out-of-repo path reference — the composition-root shape #468 added for
// `codefly run` — also renders through the GitOps module bundle pipeline, instead
// of being refused for living outside the workspace.
func TestRenderModuleBundleStagesReferencedOutOfRepoModule(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	external := t.TempDir()
	moduleDir := filepath.Join(external, "payments")

	workspace, err := resources.LoadWorkspaceFromDir(ctx, writeReferencedModuleWorkspace(t, root, moduleDir))
	if err != nil {
		t.Fatal(err)
	}
	module, err := workspace.LoadModuleFromName(ctx, "payments")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := orchestration.SelectEnvironment(workspace, "production")
	if err != nil {
		t.Fatal(err)
	}

	installModuleBundleGenerator(t)

	destination := t.TempDir()
	if err := renderModuleBundle(ctx, workspace, module, environment, destination, promotableServiceGraph("payments", []string{"api"})); err != nil {
		t.Fatalf("render referenced module bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "overlays", "production", "kustomization.yaml")); err != nil {
		t.Fatalf("referenced module bundle overlay missing: %v", err)
	}
}

// writeReferencedModuleWorkspace lays down a modules-layout workspace whose only
// module, "payments", is declared by an absolute path reference to moduleDir. It
// returns the workspace root and leaves the referenced module tree on disk.
func writeReferencedModuleWorkspace(t *testing.T, root, moduleDir string) string {
	t.Helper()
	if err := os.MkdirAll(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	module := `kind: module
name: payments
agent:
  kind: codefly:module
  publisher: codefly.dev
  name: gitops-test
  version: 1.0.0
services:
  - name: api
`
	if err := os.WriteFile(filepath.Join(moduleDir, resources.ModuleConfigurationName), []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`name: workspace
layout: modules
modules:
  - name: payments
    path: %s
environments:
  - name: production
    namespace: payments
    cluster:
      kind: k3d
`, moduleDir)
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// installModuleBundleGenerator installs a codefly:module agent that emits a
// single-environment bundle for the "payments" module.
func installModuleBundleGenerator(t *testing.T) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	agent := &resources.Agent{Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "gitops-test", Version: "1.0.0"}
	binary, err := agent.Path(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	generator := `#!/bin/sh
set -eu
module_dir="$1"
destination="$module_dir/deployment/kustomize"
mkdir -p "$destination/overlays/production"
cat > "$destination/overlays/production/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
EOF
cat > "$destination/bundle.json" <<'EOF'
{
  "schemaVersion": "codefly.dev/module-bundle/v1",
  "module": "payments",
  "environments": [{
    "name": "production",
    "namespace": "payments",
    "cluster": "k3d",
    "resourcePath": "overlays/production",
    "services": ["api"]
  }]
}
EOF
`
	if err := os.WriteFile(binary, []byte(generator), 0o755); err != nil {
		t.Fatal(err)
	}
}
