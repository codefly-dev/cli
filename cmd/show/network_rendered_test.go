package show

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/stretchr/testify/require"
)

// renderAccounts commits a render of module "saas" for environment env whose
// accounts unit publishes grpc and the named authority endpoint on their
// deployed ports, targets rest's container port by name, and publishes no
// port for connect.
func renderAccounts(t *testing.T, root, env string) {
	t.Helper()
	unit := filepath.Join("services", "accounts")
	_, err := gitops.RenderOwnedTree(context.Background(), &gitops.RenderOptions{
		Destination: filepath.Join(root, "deployments", "modules", "saas"),
		Module:      "saas", Environment: env, Namespace: "acme", Promotable: true,
		OwnedPath: "deployments/modules/saas",
		Units: []gitops.InventoryUnit{{
			Kind: gitops.UnitKindService, Module: "saas", Name: "accounts", Path: filepath.ToSlash(unit),
			Output: &gitops.InventoryKubernetesOutput{
				// RESTRICTED, not the deprecated PROMOTABLE_GITOPS_V1. This
				// literal was r15's concrete bypass: it drove profile 2
				// through the exported renderer, which installed the
				// inventory without validating it.
				Kind: "KUSTOMIZE", Profile: "KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1",
				ContractVersion: "codefly.dev/kubernetes-manifest/v1",
				Validation: &gitops.InventoryKubernetesValidation{
					StaticValidation: "STATUS_PASSED", ServerSideValidation: "STATUS_PASSED",
					Restricted: true, Violations: []string{},
				},
			},
		}},
	}, func(_ context.Context, stage string) error {
		files := map[string]string{
			"base/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - manifests.yaml\n",
			"base/manifests.yaml": `apiVersion: v1
kind: Service
metadata:
  name: accounts
  namespace: acme
spec:
  selector:
    app: accounts
  ports:
    - name: grpc-port
      port: 9090
      targetPort: 9090
    - name: http-port
      port: 8080
      targetPort: http
    - name: grpc-authority
      port: 52893
      targetPort: 52893
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: accounts
  namespace: acme
spec:
  selector:
    matchLabels:
      app: accounts
  template:
    metadata:
      labels:
        app: accounts
    spec:
      containers:
        - name: accounts
          image: registry.example.com/acme/accounts@sha256:` + strings.Repeat("a", 64) + `
          ports:
            - name: http
              containerPort: 8080
`,
			"overlays/" + env + "/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n",
		}
		for rel, content := range files {
			full := filepath.Join(stage, unit, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

func runNetworkRenderedJSON(t *testing.T, root, env string) (networkReport, error) {
	t.Helper()
	t.Chdir(root)
	previousJSON, previousEnv, previousRendered := showNetworkJSON, showNetworkEnv, showNetworkRendered
	t.Cleanup(func() {
		showNetworkJSON, showNetworkEnv, showNetworkRendered = previousJSON, previousEnv, previousRendered
		NetworkCmd.SetOut(nil)
	})
	showNetworkJSON, showNetworkEnv, showNetworkRendered = true, env, true
	var out bytes.Buffer
	NetworkCmd.SetOut(&out)
	if err := NetworkCmd.RunE(NetworkCmd, nil); err != nil {
		return networkReport{}, err
	}
	var report networkReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report), out.String())
	return report, nil
}

// --rendered adds each endpoint's container port from the committed render:
// the targetPort of the Service port publishing its deployed port, resolved by
// name through the pod template; an endpoint the render publishes no port for
// carries none.
func TestShowNetworkJSONRenderedReportsContainerPorts(t *testing.T) {
	root := writeNetworkWorkspace(t, "saas")
	renderAccounts(t, root, "staging")
	report, err := runNetworkRenderedJSON(t, root, "staging")
	require.NoError(t, err)
	require.Len(t, report.Services, 1)
	require.Equal(t, "deployments/modules/saas", report.Services[0].Render)
	container := map[string]uint32{}
	for _, endpoint := range report.Services[0].Endpoints {
		require.NotNil(t, endpoint.DeployedPort, endpoint.Name)
		if endpoint.ContainerPort != nil {
			container[endpoint.Name] = *endpoint.ContainerPort
		}
	}
	require.Equal(t, map[string]uint32{"authority": 52893, "grpc": 9090, "rest": 8080}, container)
}

func TestShowNetworkJSONRenderedWithoutARenderCarriesNoContainerPort(t *testing.T) {
	report, err := runNetworkRenderedJSON(t, writeNetworkWorkspace(t, "saas"), "staging")
	require.NoError(t, err)
	require.Empty(t, report.Services[0].Render)
	for _, endpoint := range report.Services[0].Endpoints {
		require.Nil(t, endpoint.ContainerPort, endpoint.Name)
	}
}

func TestShowNetworkJSONRenderedRefusesARenderForAnotherEnvironment(t *testing.T) {
	root := writeNetworkWorkspace(t, "saas")
	renderAccounts(t, root, "local")
	_, err := runNetworkRenderedJSON(t, root, "staging")
	require.ErrorContains(t, err, "is for environment local, not staging")
}

func TestShowNetworkRenderedNeedsJSON(t *testing.T) {
	t.Chdir(writeNetworkWorkspace(t, "saas"))
	previousJSON, previousRendered := showNetworkJSON, showNetworkRendered
	t.Cleanup(func() { showNetworkJSON, showNetworkRendered = previousJSON, previousRendered })
	showNetworkJSON, showNetworkRendered = false, true
	require.EqualError(t, NetworkCmd.RunE(NetworkCmd, nil), "--rendered adds container ports to --json: pass --json")
}
