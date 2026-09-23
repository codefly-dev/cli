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
	apiConsumesKey         = "CODEFLY__API_CONSUMES"
	registrationSecretsKey = "CODEFLY__MODULE_REGISTRATION_SECRETS"
	identityPrefixKey      = "CODEFLY__MODULE_IDENTITY_PREFIX"
	identitySecretKey      = "CODEFLY__MODULE_IDENTITY_SECRET"
	identityAliasKey       = "CODEFLY__MODULE_REGISTRATION_SECRET"
	selfEndpointKey        = "CODEFLY__SELF_ENDPOINT__WIKI__BACKEND__HTTP__HTTP"
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
// in-cluster address are public and land in the ConfigMap it loads; its
// registration secrets are a secretKeyRef the ExternalSecret materializes from
// the environment's store. No secret value exists anywhere in the tree.
func TestDerivedInjectionRendersPublicValuesAndSecretReferencesOnly(t *testing.T) {
	env := storeEnvironment()
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "http://documents.example-documents.svc.cluster.local:8080")
	injection := serviceInjection{
		Public: map[string]string{
			apiConsumesKey:  `[{"id":"documents","as":"documents"}]`,
			selfEndpointKey: "http://backend.example-wiki.svc.cluster.local:8080",
		},
		Secrets: map[string]string{registrationSecretsKey: registrationSecretsKey},
	}

	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), injection))

	rendered := buildOverlay(t, root, env.Name)
	data := configMapData(t, rendered)
	require.Equal(t, `[{"id":"documents","as":"documents"}]`, data[apiConsumesKey])
	require.Equal(t, "http://backend.example-wiki.svc.cluster.local:8080", data[selfEndpointKey])
	require.Equal(t, "backend", data[resources.ServicePrefix], "the builder's own ConfigMap values are kept")

	values := containerEnvironment(t, rendered)
	require.Equal(t, map[string]any{"secretKeyRef": map[string]any{"name": "secret-backend", "key": registrationSecretsKey, "optional": false}}, values[registrationSecretsKey]["valueFrom"])
	require.NotContains(t, values, apiConsumesKey, "a public value lives in the ConfigMap, never shadowed by the container env")

	secret := manifestOfKind(t, rendered, kindExternalSecret)
	spec := mapField(secret.value, "spec")
	require.Equal(t, map[string]any{"name": "cell-secrets", "kind": "ClusterSecretStore"}, spec["secretStoreRef"])
	require.Equal(t, []any{map[string]any{
		"secretKey": registrationSecretsKey,
		"remoteRef": map[string]any{"key": "platform-product-backend", "property": registrationSecretsKey},
	}}, spec["data"])

	for _, doc := range rendered {
		if doc.kind == "Secret" {
			t.Fatalf("the render wrote a Secret %v; only references may enter the tree", doc.value)
		}
	}
}

// A consumed module's services carry their prefix as a value and their identity
// under both names — the canonical carrier and the deprecated alias — resolving
// to the one stored identity secret, so the store holds it once and the alias
// can never be pointed at the registration secret.
func TestDerivedInjectionResolvesTheIdentityAliasToOneStoredSecret(t *testing.T) {
	env := storeEnvironment()
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "api", "store.example-documents.svc.cluster.local:5432")
	injection := serviceInjection{
		Public:  map[string]string{identityPrefixKey: "documents"},
		Secrets: map[string]string{identitySecretKey: identitySecretKey, identityAliasKey: identitySecretKey},
	}

	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "api"}, env, scopeOf(env), injection))

	rendered := buildOverlay(t, root, env.Name)
	require.Equal(t, "documents", configMapData(t, rendered)[identityPrefixKey])
	values := containerEnvironment(t, rendered)
	for _, name := range []string{identitySecretKey, identityAliasKey} {
		ref := mapField(mapField(values[name], "valueFrom"), "secretKeyRef")
		require.Equal(t, identitySecretKey, ref["key"], "%s must resolve to the stored identity secret", name)
	}
	data := sliceField(mapField(manifestOfKind(t, rendered, kindExternalSecret).value, "spec"), "data")
	require.Len(t, data, 1, "one stored identity secret, delivered under two names")
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
		serviceInjection{Secrets: map[string]string{registrationSecretsKey: registrationSecretsKey}})
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
		"credential-named key": {Public: map[string]string{"CODEFLY__MODULE_IDENTITY_SECRET": "plaintext"}},
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
				Secrets: map[string]string{registrationSecretsKey: registrationSecretsKey},
			})
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), "derived") || strings.Contains(err.Error(), "ExternalSecret"), err.Error())
		})
	}
}

// Projection rewrites only the files it changes; a builder file it does not
// touch keeps its bytes, comments included.
func TestDerivedInjectionLeavesUntouchedFilesByteForByte(t *testing.T) {
	env := storeEnvironment()
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "backend", "documents.example:8080")
	path := filepath.Join(root, "base", "deployment.yaml")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	commented := append([]byte("# rendered by the builder\n"), original...)
	require.NoError(t, os.WriteFile(path, commented, 0o600))

	require.NoError(t, projectServiceConfiguration(t.Context(), root, &resources.Service{Name: "backend"}, env, scopeOf(env), serviceInjection{
		Public: map[string]string{apiConsumesKey: "[]"},
	}))

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(commented), string(after))
}

func TestRenderInjectionsJoinSelfEndpointsWithFederation(t *testing.T) {
	injections, err := deriveRenderInjections(t.Context(), singleModuleWorkspace(), map[string]map[string]string{
		"wiki/backend": {selfEndpointKey: "http://backend.example-wiki.svc.cluster.local:8080"},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "http://backend.example-wiki.svc.cluster.local:8080", injections.forService("wiki", "backend").Public[selfEndpointKey])
	require.Empty(t, injections.forService("wiki", "other").Public)
}
