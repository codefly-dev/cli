package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const (
	apiConsumesKey  = "CODEFLY__API_CONSUMES"
	selfEndpointKey = "CODEFLY__SELF_ENDPOINT__WIKI__BACKEND__HTTP__HTTP"
	// storedSecretKey stands for a secret carrier an injection references: the
	// key the environment's store holds the value under, never the value.
	storedSecretKey = "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__CODEFLY_INTERNAL_TOKEN"
)

// storeEnvironment is a restricted-render environment whose secrets resolve
// from one ClusterSecretStore, one remote secret per service.
func storeEnvironment() *environments.Environment {
	return &environments.Environment{
		Name:      "staging",
		Namespace: "example-wiki",
		ServiceSecrets: &environments.EnvironmentServiceSecrets{
			SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
			Defaults:    &environments.EnvironmentSecretRemoteRef{Key: "{workspace}-{module}-{service}", Property: "{key}"},
		},
	}
}

func configMapData(t *testing.T, rendered []manifest) map[string]any {
	t.Helper()
	return mapField(manifestOfKind(t, rendered, "ConfigMap").value, "data")
}

// The deployed solution's entry: its api.consumes projection and its own
// in-cluster address are public and land in the ConfigMap it loads; a secret
// carrier is a secretKeyRef the ExternalSecret materializes from the
// environment's store. No secret value exists anywhere in the tree.
func TestDerivedInjectionRendersPublicValuesAndSecretReferencesOnly(t *testing.T) {
	env := storeEnvironment()
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "http://documents.example-documents.svc.cluster.local:8080")
	injection := serviceInjection{
		Public: map[string]string{
			apiConsumesKey:  `[{"id":"documents","as":"documents"}]`,
			selfEndpointKey: "http://backend.example-wiki.svc.cluster.local:8080",
		},
		Secrets: map[string]string{storedSecretKey: storedSecretKey},
	}

	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), injection))

	rendered := buildOverlay(t, root, env.Name)
	data := configMapData(t, rendered)
	require.Equal(t, `[{"id":"documents","as":"documents"}]`, data[apiConsumesKey])
	require.Equal(t, "http://backend.example-wiki.svc.cluster.local:8080", data[selfEndpointKey])
	require.Equal(t, "backend", data[resources.ServicePrefix], "the builder's own ConfigMap values are kept")

	values := containerEnvironment(t, rendered)
	require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-backend", "key": storedSecretKey, "optional": false}}, values[storedSecretKey]["valueFrom"])
	require.NotContains(t, values, apiConsumesKey, "a public value lives in the ConfigMap, never shadowed by the container env")

	secret := manifestOfKind(t, rendered, kindExternalSecret)
	spec := mapField(secret.value, "spec")
	require.Equal(t, map[string]any{"name": "cell-secrets", "kind": "ClusterSecretStore"}, spec["secretStoreRef"])
	require.Equal(t, []any{map[string]any{
		"secretKey": storedSecretKey,
		"remoteRef": map[string]any{"key": "platform-product-backend", "property": storedSecretKey},
	}}, spec["data"])

	for _, doc := range rendered {
		if doc.kind == "Secret" {
			t.Fatalf("the render wrote a Secret %v; only references may enter the tree", doc.value)
		}
	}
}

// A secret with no store cannot be delivered. Rendering the reference anyway
// leaves the pod in CreateContainerConfigError; rendering the value would publish
// it. The render refuses, and leaves the tree as the builder wrote it.
func TestDerivedInjectionRefusesASecretWithoutAStore(t *testing.T) {
	env := storeEnvironment()
	env.ServiceSecrets = nil
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
	before, err := os.ReadFile(filepath.Join(root, "base", "deployment.yaml"))
	require.NoError(t, err)

	err = projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env),
		serviceInjection{Secrets: map[string]string{storedSecretKey: storedSecretKey}})
	require.ErrorContains(t, err, "declares no service secret store")

	after, err := os.ReadFile(filepath.Join(root, "base", "deployment.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

// Strongest-security bias: a public carrier whose name core classifies as
// credential-bearing is a misfiled secret, and a render is published, so it is
// refused before anything is written. Self-endpoint carriers are exempt by name
// (an "auth-gateway" service trips the AUTH marker) but not by value.
func TestDerivedInjectionRefusesACredentialAsAPublicValue(t *testing.T) {
	for name, injection := range map[string]serviceInjection{
		"credential-named key": {Public: map[string]string{storedSecretKey: "plaintext"}},
		"self endpoint with userinfo": {Public: map[string]string{
			"CODEFLY__SELF_ENDPOINT__HOST__AUTH_GATEWAY__REST__REST": "http://user:pass@auth-gateway.example.svc.cluster.local:8080",
		}},
		"value and secret at once": {
			Public:  map[string]string{apiConsumesKey: "[]"},
			Secrets: map[string]string{apiConsumesKey: apiConsumesKey},
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := storeEnvironment()
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
			require.Error(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), injection))
		})
	}

	env := storeEnvironment()
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "auth-gateway", "documents.example:8080")
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "auth-gateway"}, env, scopeOf(env), serviceInjection{
		Public: map[string]string{"CODEFLY__SELF_ENDPOINT__HOST__AUTH_GATEWAY__REST__REST": "http://auth-gateway.example.svc.cluster.local:8080"},
	}))
}

// The derived value must be the one the process reads: a builder value that
// disagrees, or an overlay that drops or repoints a derived carrier, fails the
// render instead of shipping a deployment that silently never federates.
func TestDerivedInjectionRejectsDisagreementAndOverlayOverrides(t *testing.T) {
	t.Run("builder disagrees", func(t *testing.T) {
		env := storeEnvironment()
		root := t.TempDir()
		writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
		err := projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), serviceInjection{
			Public: map[string]string{"CODEFLY__ENDPOINT__PAYMENTS__STORE__TCP__TCP": "elsewhere.example:5432"},
		})
		require.ErrorContains(t, err, "disagrees")
	})
	for name, patch := range map[string]struct{ kind, patch string }{
		"ConfigMap value":  {"ConfigMap", "- op: replace\n  path: /data/" + apiConsumesKey + "\n  value: '[]'\n"},
		"secret reference": {kindDeployment, "- op: replace\n  path: /spec/template/spec/containers/0/env/0/valueFrom/secretKeyRef/key\n  value: OTHER\n"},
	} {
		t.Run(name, func(t *testing.T) {
			env := storeEnvironment()
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
			addProjectionPatch(t, root, env.Name, patch.kind, patch.patch)
			err := projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), serviceInjection{
				Public:  map[string]string{apiConsumesKey: `[{"id":"documents"}]`},
				Secrets: map[string]string{storedSecretKey: storedSecretKey},
			})
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "derived") || strings.Contains(err.Error(), "ExternalSecret"), err.Error())
		})
	}
}

// Projection rewrites only the files it changes; a builder file it does not
// touch keeps its bytes, comments included.
//
// The baseline is the same tree projected with no derived carriers at all, so
// what this pins is the carrier projection's own footprint rather than every
// other projection's: a deployed render also binds the workload to its own
// service account, which writes the Deployment whatever the carriers are.
func TestDerivedInjectionLeavesUntouchedFilesByteForByte(t *testing.T) {
	env := storeEnvironment()
	comment := []byte("# rendered by the builder\n")
	commentedTree := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
		path := filepath.Join(root, "base", "deployment.yaml")
		original, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(comment, original...), 0o600))
		return root
	}

	baselineRoot := commentedTree(t)
	require.NoError(t, projectServiceConfiguration(t.Context(), baselineRoot, &resources.Service{Name: "backend"}, env, scopeOf(env), serviceInjection{}))
	baseline, err := os.ReadFile(filepath.Join(baselineRoot, "base", "deployment.yaml"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(baseline), string(comment)), "the builder's comment survives the render")

	root := commentedTree(t)
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), serviceInjection{
		Public: map[string]string{apiConsumesKey: "[]"},
	}))

	after, err := os.ReadFile(filepath.Join(root, "base", "deployment.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(baseline), string(after))
}

// A deployed render holds the composition's solutions to the environment's
// declared surface, before it derives any carrier for them; the same composition
// renders on a local cluster, which has no rules for the boundary to hold.
func TestDeployedRenderRefusesASolutionPathAroundItsHost(t *testing.T) {
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), filepath.Join("..", "solutionrun", "testdata", "solution-boundary"))
	require.NoError(t, err)

	// The environment fixes a public origin, so the composition reaches the
	// boundary rule rather than stopping at the origin one.
	env := storeEnvironment()
	env.DNS = &environments.EnvironmentDNS{AppHostSuffix: "cell.example.com"}
	_, err = deriveRenderInjections(t.Context(), workspace, env, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "consumer/backend depends on bridge/relay")

	// The surface the environment declares is the one the render reads: naming
	// the host's doors admits them and nothing else.
	env.SolutionBoundary = &environments.EnvironmentSolutionBoundary{
		HostSurface: []environments.EnvironmentHostSurfaceEntry{
			{Service: "host/frontend", Endpoint: "http"},
			{Service: "host/gateway", Endpoint: "rest"},
		},
	}
	_, err = deriveRenderInjections(t.Context(), workspace, env, nil, nil)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "depends on host/frontend")
	require.NotContains(t, err.Error(), "depends on host/gateway")
	require.Contains(t, err.Error(), "depends on bridge/relay")

	local := &environments.Environment{Name: "local", Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d}}
	_, err = deriveRenderInjections(t.Context(), workspace, local, nil, nil)
	require.NoError(t, err)
}

// A workspace composing no solution derives nothing of its own, so the render's
// injections are exactly the self-endpoint carriers of the services it deployed.
func TestRenderInjectionsJoinSelfEndpoints(t *testing.T) {
	injections, err := deriveRenderInjections(t.Context(), singleModuleWorkspace(), storeEnvironment(), map[string]map[string]string{
		"wiki/backend": {selfEndpointKey: "http://backend.example-wiki.svc.cluster.local:8080"},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "http://backend.example-wiki.svc.cluster.local:8080", injections.forService("wiki", "backend").Public[selfEndpointKey])
	require.Empty(t, injections.forService("wiki", "other").Public)
}

// A vendor image (redis) declares no CODEFLY__SERVICE, so no container claims
// the service. Its self endpoint has no Codefly-aware reader, so the render
// leaves the tree untouched instead of refusing; a required carrier (a
// solution's consumed routes) for the same unclaimed service still refuses.
func TestDerivedInjectionSkipsOfferedCarriersNoContainerClaims(t *testing.T) {
	env := storeEnvironment()
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
	// The baseline is the tree with no derived carriers: a deployed render binds
	// the workload to its own service account either way, and what this pins is
	// that an unread carrier adds nothing on top of that.
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "cache"}, env, scopeOf(env), serviceInjection{}))
	before, err := os.ReadFile(filepath.Join(root, "base", "deployment.yaml"))
	require.NoError(t, err)

	selfOnly := serviceInjection{Public: map[string]string{
		"CODEFLY__SELF_ENDPOINT__SAAS__CACHE__READ__TCP": "tcp://cache.example-saas.svc.cluster.local:6379",
	}}
	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "cache"}, env, scopeOf(env), selfOnly))
	after, err := os.ReadFile(filepath.Join(root, "base", "deployment.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))

	required := serviceInjection{Public: map[string]string{apiConsumesKey: `[{"id":"documents"}]`}}
	err = projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "cache"}, env, scopeOf(env), required)
	require.ErrorContains(t, err, "no rendered container declares")
}
