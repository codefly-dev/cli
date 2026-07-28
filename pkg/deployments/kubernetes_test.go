package deployments

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestVerifyLocalK3dTargetRejectsRemoteKindsBeforeInspectingKubeconfig(t *testing.T) {
	for _, kind := range []string{"eks", "gke", "aks", "external"} {
		t.Run(kind, func(t *testing.T) {
			env := &resources.Environment{
				Name: "production",
				Cluster: &resources.EnvironmentCluster{
					Kind:       kind,
					Kubeconfig: filepath.Join(t.TempDir(), "config"),
					Context:    "k3d-production",
				},
			}

			_, err := VerifyLocalK3dTarget(context.Background(), env)
			require.Error(t, err)
			require.Contains(t, err.Error(), "exact local k3d target")
			require.Contains(t, err.Error(), "--render-only")
			require.Contains(t, err.Error(), "GitOps")
		})
	}
}

func TestVerifyLocalK3dTargetRejectsStaleCurrentContext(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.writeSelected(kubeconfigDocument("eks-production", "eks-production", "production", "https://eks.example.com"))
	harness.writeOwned(kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443"))

	env := harness.environment("k3d-dev")
	_, err := VerifyLocalK3dTarget(context.Background(), env)

	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match")
	require.Contains(t, err.Error(), "eks-production")
}

func TestVerifyLocalK3dTargetRejectsMismatchedKubeconfigServer(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.writeSelected(kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443"))
	harness.writeOwned(kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6553"))

	_, err := VerifyLocalK3dTarget(context.Background(), harness.environment("k3d-dev"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match k3d-owned cluster")
}

func TestVerifyLocalK3dTargetRejectsRenamedNonK3dContext(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	harness.writeSelected(kubeconfigDocument("k3d-production", "k3d-production", "production", "https://eks.example.com"))
	harness.writeOwned(kubeconfigDocument("k3d-production", "k3d-production", "k3d-production", "https://127.0.0.1:6443"))

	_, err := VerifyLocalK3dTarget(context.Background(), harness.environment("k3d-production"))

	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match k3d-owned cluster")
}

func TestVerifyLocalK3dTargetBindsExactIdentity(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	config := kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443")
	harness.writeSelected(config)
	harness.writeOwned(config)

	target, err := VerifyLocalK3dTarget(context.Background(), harness.environment("k3d-dev"))

	require.NoError(t, err)
	require.Equal(t, VerifiedKubernetesTarget{
		Kind:       "k3d",
		Kubeconfig: harness.kubeconfig,
		Context:    "k3d-dev",
		Cluster:    "k3d-dev",
		APIServer:  "https://127.0.0.1:6443",
		K3dCluster: "dev",
	}, target)
}

func TestKubernetesApplyRejectsContextSwapAfterValidation(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	dev := kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443")
	harness.writeSelected(dev)
	harness.writeOwned(dev)
	env := harness.environment("")

	target, err := VerifyLocalK3dTarget(context.Background(), env)
	require.NoError(t, err)

	other := kubeconfigDocument("k3d-other", "k3d-other", "k3d-other", "https://127.0.0.1:7443")
	harness.writeSelected(other)
	harness.writeOwned(other)

	err = KubernetesApply(context.Background(), env, target, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: blocked\n")
	require.Error(t, err)
	require.Contains(t, err.Error(), "target changed after validation")
	require.NoFileExists(t, harness.applyLog)
}

func TestLocalApplyManagerFailsClosedBeforeApplyWhenImagePreparationFails(t *testing.T) {
	for _, test := range []struct {
		name        string
		pullFails   bool
		importFails bool
	}{
		{name: "pull failure", pullFails: true},
		{name: "import failure", importFails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newKubernetesCommandHarness(t)
			config := kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443")
			harness.writeSelected(config)
			harness.writeOwned(config)
			if test.pullFails {
				t.Setenv("FAKE_DOCKER_INSPECT_FAIL", "1")
				t.Setenv("FAKE_DOCKER_PULL_FAIL", "1")
			}
			if test.importFails {
				t.Setenv("FAKE_K3D_IMPORT_FAIL", "1")
			}

			workspace, module, service := deploymentFixture(t)
			manager, err := NewLocalApplyManager(context.Background(), workspace, harness.environment("k3d-dev"))
			require.NoError(t, err)

			err = manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput())
			require.Error(t, err)
			require.NoFileExists(t, harness.applyLog)
			require.NoFileExists(t, harness.kustomizeLog)
		})
	}
}

func TestLocalApplyManagerRecordsTargetAndRenderedTreeDigest(t *testing.T) {
	harness := newKubernetesCommandHarness(t)
	config := kubeconfigDocument("k3d-dev", "k3d-dev", "k3d-dev", "https://127.0.0.1:6443")
	harness.writeSelected(config)
	harness.writeOwned(config)
	workspace, module, service := deploymentFixture(t)
	manager, err := NewLocalApplyManager(context.Background(), workspace, harness.environment("k3d-dev"))
	require.NoError(t, err)

	require.NoError(t, manager.Handle(context.Background(), service, module, kubernetesDeploymentOutput()))

	digest, ok := manager.RenderedDigest(context.Background(), module, service)
	require.True(t, ok)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)
	require.Equal(t, "https://127.0.0.1:6443", manager.Target().APIServer)
	require.FileExists(t, harness.applyLog)
}

func TestRenderedTreeDigestBindsPathsAndContents(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	writeTestFile(t, filepath.Join(first, "base", "deployment.yaml"), "kind: Deployment\n")
	writeTestFile(t, filepath.Join(first, "overlays", "local", "kustomization.yaml"), "resources: []\n")
	writeTestFile(t, filepath.Join(second, "overlays", "local", "kustomization.yaml"), "resources: []\n")
	writeTestFile(t, filepath.Join(second, "base", "deployment.yaml"), "kind: Deployment\n")

	firstDigest, err := RenderedTreeDigest(first)
	require.NoError(t, err)
	secondDigest, err := RenderedTreeDigest(second)
	require.NoError(t, err)
	require.Equal(t, firstDigest, secondDigest)

	writeTestFile(t, filepath.Join(second, "base", "deployment.yaml"), "kind: StatefulSet\n")
	changedDigest, err := RenderedTreeDigest(second)
	require.NoError(t, err)
	require.NotEqual(t, firstDigest, changedDigest)
}

type kubernetesCommandHarness struct {
	t            *testing.T
	selected     string
	owned        string
	kubeconfig   string
	applyLog     string
	kustomizeLog string
}

func newKubernetesCommandHarness(t *testing.T) kubernetesCommandHarness {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	harness := kubernetesCommandHarness{
		t:            t,
		selected:     filepath.Join(root, "selected.json"),
		owned:        filepath.Join(root, "owned.yaml"),
		kubeconfig:   filepath.Join(root, "declared-kubeconfig"),
		applyLog:     filepath.Join(root, "apply.log"),
		kustomizeLog: filepath.Join(root, "kustomize.log"),
	}
	writeTestFile(t, harness.kubeconfig, "fixture")
	writeExecutable(t, filepath.Join(bin, "kubectl"), `#!/bin/sh
case " $* " in
  *" config view "*)
    cat "$FAKE_SELECTED_KUBECONFIG"
    ;;
  *" apply "*)
    if [ "$2" = "$FAKE_DECLARED_KUBECONFIG" ] || [ ! -f "$2" ]; then
      exit 93
    fi
    printf '%s\n' "$*" >> "$FAKE_APPLY_LOG"
    cat >/dev/null
    printf 'resource configured\n'
    ;;
  *)
    exit 90
    ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "k3d"), `#!/bin/sh
if [ "$1" = "kubeconfig" ] && [ "$2" = "get" ]; then
  cat "$FAKE_K3D_KUBECONFIG"
  exit 0
fi
if [ "$1" = "image" ] && [ "$2" = "import" ]; then
  if [ "$FAKE_K3D_IMPORT_FAIL" = "1" ]; then
    exit 41
  fi
  exit 0
fi
exit 91
`)
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/bin/sh
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  if [ "$FAKE_DOCKER_INSPECT_FAIL" = "1" ]; then
    exit 42
  fi
  exit 0
fi
if [ "$1" = "pull" ]; then
  if [ "$FAKE_DOCKER_PULL_FAIL" = "1" ]; then
    exit 43
  fi
  exit 0
fi
exit 92
`)
	writeExecutable(t, filepath.Join(bin, "kustomize"), `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_KUSTOMIZE_LOG"
printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: applied\n'
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SELECTED_KUBECONFIG", harness.selected)
	t.Setenv("FAKE_K3D_KUBECONFIG", harness.owned)
	t.Setenv("FAKE_DECLARED_KUBECONFIG", harness.kubeconfig)
	t.Setenv("FAKE_APPLY_LOG", harness.applyLog)
	t.Setenv("FAKE_KUSTOMIZE_LOG", harness.kustomizeLog)
	return harness
}

func (h kubernetesCommandHarness) environment(contextName string) *resources.Environment {
	return &resources.Environment{
		Name: "local",
		Cluster: &resources.EnvironmentCluster{
			Kind:       "k3d",
			Kubeconfig: h.kubeconfig,
			Context:    contextName,
		},
	}
}

func (h kubernetesCommandHarness) writeSelected(content string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.selected, []byte(content), 0o600))
}

func (h kubernetesCommandHarness) writeOwned(content string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.owned, []byte(content), 0o600))
}

func kubeconfigDocument(currentContext, contextName, clusterName, apiServer string) string {
	document := map[string]any{
		"apiVersion":      "v1",
		"current-context": currentContext,
		"contexts": []any{map[string]any{
			"name": contextName,
			"context": map[string]any{
				"cluster": clusterName,
			},
		}},
		"clusters": []any{map[string]any{
			"name": clusterName,
			"cluster": map[string]any{
				"server": apiServer,
			},
		}},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func deploymentFixture(t *testing.T) (*resources.Workspace, *resources.Module, *resources.Service) {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), `name: target-test
layout: modules
modules:
  - name: backend
`)
	writeTestFile(t, filepath.Join(root, "modules", "backend", "module.codefly.yaml"), `kind: module
name: backend
services:
  - name: api
`)
	writeTestFile(t, filepath.Join(root, "modules", "backend", "services", "api", "service.codefly.yaml"), `kind: service
name: api
version: 0.0.0
module: backend
agent:
  kind: runtime::service
  name: test
  version: 0.0.0
  publisher: codefly.dev
`)
	writeTestFile(t, filepath.Join(root, "deployments", "modules", "backend", "services", "api", "overlays", "local", "kustomization.yaml"), `images:
  - name: app
    newName: app
    newTag: test
`)
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(context.Background(), "backend")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(context.Background(), "api")
	require.NoError(t, err)
	return workspace, module, service
}

func kubernetesDeploymentOutput() *builderv0.DeploymentOutput {
	return &builderv0.DeploymentOutput{
		Kind: &builderv0.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeploymentOutput{
				Kind: builderv0.KubernetesDeploymentOutput_KUSTOMIZE,
			},
		},
	}
}

func writeExecutable(t *testing.T, filePath, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0o700))
}

func writeTestFile(t *testing.T, filePath, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filePath), 0o755))
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0o600), fmt.Sprintf("write %s", filePath))
}
