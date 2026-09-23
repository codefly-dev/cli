package environments_test

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

func TestModuleNamespaceSuffixesOnlyWhenSeveralModulesAreComposed(t *testing.T) {
	env := &environments.Environment{Name: "staging", Namespace: "platform"}
	one := &resources.Workspace{Name: "acme", Layout: resources.LayoutKindModules, Modules: []*resources.ModuleReference{{Name: "saas"}}}
	flat := &resources.Workspace{Name: "acme", Layout: resources.LayoutKindFlat}
	several := &resources.Workspace{Name: "acme", Layout: resources.LayoutKindModules, Modules: []*resources.ModuleReference{{Name: "saas"}, {Name: "documents"}}}

	require.Equal(t, "platform", env.ModuleNamespace(one, "saas"))
	require.Equal(t, "platform", env.ModuleNamespace(flat, "acme"))
	require.Equal(t, "platform-saas", env.ModuleNamespace(several, "saas"))
	require.Equal(t, "platform-documents", env.ModuleNamespace(several, "documents"))

	// No declared namespace: nothing to derive from, so callers keep their own rule.
	undeclared := &environments.Environment{Name: "local"}
	require.Empty(t, undeclared.ModuleNamespace(several, "saas"))
	var nilEnv *environments.Environment
	require.Empty(t, nilEnv.ModuleNamespace(several, "saas"))
}

// The suffix can push a namespace that was a valid label on its own past what
// Kubernetes accepts; that must fail at workspace load, naming the rule, rather
// than when Argo CD applies the render.
func TestValidateWorkspaceRejectsDerivedModuleNamespaceThatIsNotALabel(t *testing.T) {
	root := t.TempDir()
	workspace := `name: acme
layout: modules
modules:
  - name: saas
  - name: documents
environments:
  - name: staging
    namespace: ` + strings.Repeat("a", 60) + `
`
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644))
	for _, module := range []string{"saas", "documents"} {
		dir := filepath.Join(root, "modules", module)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, resources.ModuleConfigurationName), []byte("kind: module\nname: "+module+"\n"), 0o644))
	}
	loaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	err = environments.ValidateWorkspace(context.Background(), loaded)
	require.Error(t, err)
	require.Contains(t, err.Error(), "module namespace")
	require.Contains(t, err.Error(), "<namespace>-<module>")

	// The same namespace is fine for a single module: nothing is suffixed.
	loaded.Modules = loaded.Modules[:1]
	require.NoError(t, environments.ValidateWorkspace(context.Background(), loaded))
}

func TestServiceSecretsRemoteRefResolvesInOrder(t *testing.T) {
	scope := environments.SecretScope{Workspace: "acme", Module: "documents", Service: "store"}
	secrets := &environments.EnvironmentServiceSecrets{
		SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell", Kind: "ClusterSecretStore"},
		Defaults:    &environments.EnvironmentSecretRemoteRef{Key: "{workspace}-{module}-{service}", Property: "{key}"},
		Services: map[string]environments.EnvironmentServiceSecretMapping{
			"store": {
				RemoteKeys: map[string]environments.EnvironmentSecretRemoteRef{"EXPLICIT": {Key: "shared/explicit", Property: "field"}},
			},
			"accounts": {
				Defaults: &environments.EnvironmentSecretRemoteRef{Key: "{module}/{service}/{key}"},
			},
		},
	}
	require.NoError(t, secrets.Validate())

	// An explicit remote-key wins over every template.
	require.Equal(t, environments.EnvironmentSecretRemoteRef{Key: "shared/explicit", Property: "field"}, secrets.RemoteRef(scope, "EXPLICIT"))
	// A service with no defaults of its own resolves through the environment's,
	// with the module substituted so two modules' "store" differ.
	require.Equal(t, environments.EnvironmentSecretRemoteRef{Key: "acme-documents-store", Property: "DB_PASSWORD"}, secrets.RemoteRef(scope, "DB_PASSWORD"))
	saas := environments.SecretScope{Workspace: "acme", Module: "saas", Service: "store"}
	require.Equal(t, environments.EnvironmentSecretRemoteRef{Key: "acme-saas-store", Property: "DB_PASSWORD"}, secrets.RemoteRef(saas, "DB_PASSWORD"))
	// A service's own defaults win over the environment's.
	accounts := environments.SecretScope{Workspace: "acme", Module: "saas", Service: "accounts"}
	require.Equal(t, environments.EnvironmentSecretRemoteRef{Key: "saas/accounts/TOKEN"}, secrets.RemoteRef(accounts, "TOKEN"))
	// Nothing declared: the "<service>/<key>" path, also for a nil receiver.
	bare := &environments.EnvironmentServiceSecrets{SecretStore: secrets.SecretStore}
	require.Equal(t, environments.EnvironmentSecretRemoteRef{Key: "store/TOKEN"}, bare.RemoteRef(scope, "TOKEN"))
	var none *environments.EnvironmentServiceSecrets
	require.Equal(t, environments.EnvironmentSecretRemoteRef{Key: "store/TOKEN"}, none.RemoteRef(scope, "TOKEN"))
}

func TestServiceSecretsEnvironmentDefaultsValidate(t *testing.T) {
	store := environments.EnvironmentSecretStoreReference{Name: "cell", Kind: "ClusterSecretStore"}
	valid := &environments.EnvironmentServiceSecrets{SecretStore: store, Defaults: &environments.EnvironmentSecretRemoteRef{Key: "{workspace}-{module}-{service}-{key}"}}
	require.NoError(t, valid.Validate())

	empty := &environments.EnvironmentServiceSecrets{SecretStore: store, Defaults: &environments.EnvironmentSecretRemoteRef{Key: " "}}
	require.ErrorContains(t, empty.Validate(), "defaults key cannot be empty")

	typo := &environments.EnvironmentServiceSecrets{SecretStore: store, Defaults: &environments.EnvironmentSecretRemoteRef{Key: "{modul}-{service}"}}
	err := typo.Validate()
	require.ErrorContains(t, err, "{modul}")
	require.ErrorContains(t, err, "{module}")

	property := &environments.EnvironmentServiceSecrets{SecretStore: store, Defaults: &environments.EnvironmentSecretRemoteRef{Key: "{service}", Property: "{ky}"}}
	require.ErrorContains(t, property.Validate(), "defaults property")
}

// The top-level defaults load from the workspace YAML like the per-service ones.
func TestEnvironmentServiceSecretsEnvironmentDefaultsLoadFromWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := `name: acme
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: cell-secrets
        kind: ClusterSecretStore
      defaults:
        key: "{module}-{service}"
        property: "{key}"
      services:
        accounts:
          remote-keys:
            TOKEN: shared/token
`
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644))
	loaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	env, err := environments.Select(loaded, "prod")
	require.NoError(t, err)
	require.NotNil(t, env.ServiceSecrets.Defaults)
	require.Equal(t, "{module}-{service}", env.ServiceSecrets.Defaults.Key)
	require.Equal(t, "{key}", env.ServiceSecrets.Defaults.Property)
	require.Equal(t, "shared/token", env.ServiceSecrets.Services["accounts"].RemoteKeys["TOKEN"].Key)
}
