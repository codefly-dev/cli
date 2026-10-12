package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// accountEnvironment is a deployed environment with no managed service, so the
// only thing deciding an account is the rule itself.
func accountEnvironment() *environments.Environment {
	env := consumerEnvironment(managedIdentityService())
	delete(env.ManagedServices, "store")
	return env
}

// appendToDeployment adds lines to a rendered service's Deployment, which is how
// these tests express what an agent or an overlay already did.
func appendToDeployment(t *testing.T, root string, lines string) {
	t.Helper()
	path := filepath.Join(root, "base", "deployment.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte(lines)...), 0o644))
}

// patchOverlay adds a kustomize patch to a rendered service's environment
// overlay, which is how these tests express what the cluster finally receives.
func patchOverlay(t *testing.T, root, environment, patch string) {
	t.Helper()
	path := filepath.Join(root, "overlays", environment, "kustomization.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte(patch)...), 0o644))
}

// Two workloads in one namespace are two principals. A rule that admits one of
// them must not admit the other, which it cannot express while both run under
// the account every pod of the namespace shares.
func TestDeployedWorkloadsGetDistinctServiceAccounts(t *testing.T) {
	env := accountEnvironment()
	accounts := map[string]string{}
	for _, service := range []string{"frontend", "telemetry"} {
		root := filepath.Join(t.TempDir(), service)
		writeConsumerTree(t, root, env.Name, env.Namespace, service, "store.example.test:5432")
		require.NoError(t, projectManagedIdentity(
			context.Background(), root, &resources.Service{Name: service}, env, consumerScope(),
		))
		rendered := buildOverlay(t, root, env.Name)
		account := manifestOfKind(t, rendered, "ServiceAccount")
		metadata := mapField(account.value, "metadata")
		require.Equal(t, env.Namespace, metadata["namespace"])
		name, _ := metadata["name"].(string)
		accounts[service] = name

		spec, ok := podSpec(manifestOfKind(t, rendered, kindDeployment))
		require.True(t, ok)
		require.Equal(t, name, spec["serviceAccountName"], "the workload binds the account rendered for it")
	}
	require.Equal(t, map[string]string{"frontend": "frontend", "telemetry": "telemetry"}, accounts,
		"each workload is named by its own account, so no two share a principal")
}

// An account name a workload already carries is not evidence of a distinct
// identity: two services bound to one name hold each other's grants, and the
// shared default is that case at its worst. What the render requires is an
// account this service's own render defines.
func TestAWorkloadMustRunUnderAnAccountItsOwnRenderDefines(t *testing.T) {
	for name, tc := range map[string]struct{ bound, refusal string }{
		"the namespace default": {bound: "default", refusal: "runs under the account every pod of its namespace shares"},
		"a shared account":      {bound: "shared-runtime", refusal: `runs under account "shared-runtime", which this service's render does not define`},
	} {
		t.Run(name, func(t *testing.T) {
			env := accountEnvironment()
			root := filepath.Join(t.TempDir(), "frontend")
			writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "store.example.test:5432")
			appendToDeployment(t, root, "      serviceAccountName: "+tc.bound+"\n")

			err := projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "frontend"}, env, consumerScope(), serviceInjection{})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.refusal)
			require.Contains(t, err.Error(), "Deployment/frontend")
		})
	}
}

// The name in the output is not the name in the source: a kustomize transform
// renames an account and every reference to it together, and both are still this
// service's own identity. The rule resolves what the unit defines rather than
// comparing the account to the service's name.
func TestARenamedAccountIsStillTheServicesOwn(t *testing.T) {
	env := accountEnvironment()
	root := filepath.Join(t.TempDir(), "frontend")
	writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "store.example.test:5432")
	require.NoError(t, projectManagedIdentity(t.Context(), root, &resources.Service{Name: "frontend"}, env, consumerScope()))
	path := filepath.Join(root, "overlays", env.Name, "kustomization.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(data, []byte("namePrefix: prefixed-\n")...), 0o644))

	require.NoError(t, requireOwnServiceAccount(root, env.Name, "payments", "frontend"))

	spec, ok := podSpec(manifestOfKind(t, buildOverlay(t, root, env.Name), kindDeployment))
	require.True(t, ok)
	require.Equal(t, "prefixed-frontend", spec["serviceAccountName"], "the transform renamed both sides together")
}

// The rule is decided on what the cluster receives. A workload reached through a
// differently-suffixed file or a nested resource directory is one the writer
// cannot reach, and an overlay can put the shared account back after it ran —
// each of those left a workload on the shared account while the base tree looked
// conforming.
func TestServiceAccountIsDecidedOnTheEffectiveRender(t *testing.T) {
	for _, shape := range []string{"other suffix", "nested directory", "overlay patch"} {
		t.Run(shape, func(t *testing.T) {
			env := accountEnvironment()
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "store.example.test:5432")
			base := filepath.Join(root, "base")
			switch shape {
			case "other suffix", "nested directory":
				replacement := "deployment.yml"
				if shape == "nested directory" {
					replacement = "nested/deployment.yaml"
					require.NoError(t, os.MkdirAll(filepath.Join(base, "nested"), 0o755))
				}
				require.NoError(t, os.Rename(filepath.Join(base, "deployment.yaml"), filepath.Join(base, replacement)))
				path := filepath.Join(base, "kustomization.yaml")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "deployment.yaml", replacement)), 0o644))
			case "overlay patch":
				patchOverlay(t, root, env.Name, "patches:\n  - target:\n      kind: Deployment\n      name: frontend\n    patch: |-\n      - op: add\n        path: /spec/template/spec/serviceAccountName\n        value: default\n")
			}

			err := projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "frontend"}, env, consumerScope(), serviceInjection{})
			require.Error(t, err, "the effective render leaves the workload on an identity that is not its own")
			require.Contains(t, err.Error(), "Deployment/frontend")
		})
	}
}

// A developer's own cluster has no rules naming these principals, so a local
// render is left exactly as its agents wrote it.
func TestLocalRenderLeavesTheWorkloadOnTheNamespaceDefault(t *testing.T) {
	env := &environments.Environment{
		Name:      "local",
		Namespace: "payments",
		Cluster:   &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d},
	}
	root := filepath.Join(t.TempDir(), "frontend")
	writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "store.example.test:5432")
	before := readTree(t, root)

	require.NoError(t, projectManagedIdentity(
		context.Background(), root, &resources.Service{Name: "frontend"}, env, consumerScope(),
	))
	require.Equal(t, before, readTree(t, root))
}

// A unit with no workload has no identity to separate from another's, so it
// earns neither an account nor a refusal.
func TestServiceAccountProjectionSkipsAUnitWithNoWorkload(t *testing.T) {
	env := accountEnvironment()
	root := t.TempDir()
	base := filepath.Join(root, "base")
	overlay := filepath.Join(root, "overlays", env.Name)
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(base, "config-map.yaml"),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n  namespace: "+env.Namespace+"\ndata:\n  A: \"1\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - config-map.yaml\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../base\n"), 0o644))
	before := readTree(t, root)

	require.NoError(t, projectManagedIdentity(
		context.Background(), root, &resources.Service{Name: "settings"}, env, consumerScope(),
	))
	require.Equal(t, before, readTree(t, root))
}

// A workload its agent already bound to the service's own name is already
// nameable, so the projection leaves the tree alone and the rule still holds.
func TestServiceAccountProjectionKeepsAnAgentsOwnAccount(t *testing.T) {
	env := accountEnvironment()
	root := filepath.Join(t.TempDir(), "gateway")
	writeConsumerTree(t, root, env.Name, env.Namespace, "gateway", "store.example.test:5432")
	appendToDeployment(t, root, "      serviceAccountName: gateway\n")
	before := readTree(t, root)

	require.NoError(t, projectManagedIdentity(
		context.Background(), root, &resources.Service{Name: "gateway"}, env, consumerScope(),
	))
	require.Equal(t, before, readTree(t, root))
}

func TestServiceAccountProjectionIsIdempotent(t *testing.T) {
	env := accountEnvironment()
	root := filepath.Join(t.TempDir(), "frontend")
	writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "store.example.test:5432")

	service := &resources.Service{Name: "frontend"}
	require.NoError(t, projectManagedIdentity(context.Background(), root, service, env, consumerScope()))
	once := readTree(t, root)
	require.NoError(t, projectManagedIdentity(context.Background(), root, service, env, consumerScope()))
	require.Equal(t, once, readTree(t, root))
}

// Within one namespace a principal belongs to one service. Two services of a
// render claiming the same account is the collapse the rule exists to stop, and
// only the whole render can see it.
func TestNamespaceAccountsAreClaimedByOneService(t *testing.T) {
	claimed := namespaceAccounts{}
	require.NoError(t, claimed.record("payments", "frontend", "saas/frontend"))
	require.NoError(t, claimed.record("payments", "frontend", "saas/frontend"), "the same service may bind its account on several workloads")
	require.NoError(t, claimed.record("other", "frontend", "shop/frontend"), "another namespace is another principal")

	err := claimed.record("payments", "frontend", "shop/frontend")
	require.Error(t, err)
	require.Contains(t, err.Error(), "saas/frontend")
	require.Contains(t, err.Error(), "shop/frontend")
	require.Contains(t, err.Error(), `namespace "payments"`)
}

func TestBaseBindsAWorkload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "frontend")
	writeConsumerTree(t, root, "production", "payments", "frontend", "store.example.test:5432")
	binds, err := baseBindsAWorkload(filepath.Join(root, "base"))
	require.NoError(t, err)
	require.True(t, binds)

	require.NoError(t, os.Rename(filepath.Join(root, "base", "deployment.yaml"), filepath.Join(root, "base", "deployment.yml")))
	binds, err = baseBindsAWorkload(filepath.Join(root, "base"))
	require.NoError(t, err)
	require.False(t, binds, "the writer reads only the YAML files directly in base")

	missing, err := baseBindsAWorkload(filepath.Join(root, "nowhere"))
	require.NoError(t, err)
	require.False(t, missing)
}
