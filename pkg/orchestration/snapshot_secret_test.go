package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// staticConfigurationLoader feeds a declared configuration into a real
// configurations.Manager so the secret-resolution path can be exercised without
// a live provider. It mirrors the declared reference-only manifests a workspace
// commits: keys and provider references, never plaintext values.
type staticConfigurationLoader struct {
	confs []*basev0.Configuration
}

func (staticConfigurationLoader) Identity() string { return "static-test-loader" }

func (staticConfigurationLoader) Load(context.Context, *resources.Environment) error { return nil }

func (l staticConfigurationLoader) Configurations() []*basev0.Configuration { return l.confs }

func (staticConfigurationLoader) DNS() []*basev0.DNS { return nil }

func declaredSecretConfiguration() *basev0.Configuration {
	return &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "auth",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "client_secret", Value: "op://dev-vault/auth/client_secret", Secret: true},
			},
		}},
	}
}

// newSnapshotWorkspaceFlow builds a real single-service flow for a mode so the
// NewFlow wiring — not a hand-registered resolver — is what the assertions
// observe.
func newSnapshotWorkspaceFlow(t *testing.T, mode Mode) *Flow {
	t.Helper()
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": `name: value-free
layout: modules
modules:
    - name: web
environments:
    - name: local
      naming-scope: from-yaml
`,
		"modules/web/module.codefly.yaml": `kind: module
name: web
project: value-free
services:
    - name: gateway
`,
		"modules/web/services/gateway/service.codefly.yaml": `kind: service
name: gateway
version: 0.0.0
module: web
agent:
    kind: runtime::service
    name: krakend
    version: 0.0.6
    publisher: codefly.ai
endpoints:
    - name: rest
      visibility: public
      api: rest
`,
	})
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, module, service, env, mode)
	require.NoError(t, err)
	return flow
}

func TestSnapshotSecretResolverAnswersReferencesWithoutAProvider(t *testing.T) {
	ref, ok := configurations.ParseSecretReference("op://dev-vault/auth/client_secret")
	require.True(t, ok)

	resolver := snapshotSecretResolver{}
	require.Equal(t, configurations.OnePasswordScheme, resolver.Scheme())

	value, err := resolver.Resolve(context.Background(), ref)
	require.NoError(t, err)
	require.NotEmpty(t, value)
}

// snapshotSecretResolver only answers OnePasswordScheme. If core teaches
// ParseSecretReference another scheme, a SnapshotMode render would silently
// demand that provider again for that scheme — so pin the assumption: no
// non-op candidate is recognized today, and op is. A core bump that adds a
// scheme trips this and forces the resolver to grow.
func TestSnapshotSecretResolverCoversEveryReferenceSchemeCoreRecognizes(t *testing.T) {
	for _, candidate := range []string{
		"vault://secret/data/app",
		"aws-sm://us-east-1/app/secret",
		"gcp-sm://project/secret/version",
		"azure-kv://vault/secret",
	} {
		_, ok := configurations.ParseSecretReference(candidate)
		require.Falsef(t, ok, "core now recognizes %q as a secret reference; snapshotSecretResolver must cover its scheme", candidate)
	}
	_, ok := configurations.ParseSecretReference("op://vault/item/field")
	require.True(t, ok)
}

// Without a resolver and with no provider configured, resolving a reference-only
// secret fails at the resolution step — this is the "render demands local value
// files" pressure the snapshot resolver removes.
func TestManagerRejectsReferenceOnlySecretWithoutAResolver(t *testing.T) {
	ctx := context.Background()
	manager, err := configurations.NewManager(ctx, &resources.Workspace{})
	require.NoError(t, err)
	manager.WithLoader(staticConfigurationLoader{confs: []*basev0.Configuration{declaredSecretConfiguration()}})

	err = manager.Load(ctx, &resources.Environment{Name: "staging"})
	require.ErrorContains(t, err, "requires a backend that is not configured")
}

// With the snapshot resolver registered, the same reference-only declaration
// loads without a provider and the value is replaced by the inert placeholder,
// which promotableConfiguration then discards into a secretKeyRef.
func TestSnapshotSecretResolverMakesReferenceOnlyRenderValueFree(t *testing.T) {
	ctx := context.Background()
	loader := staticConfigurationLoader{confs: []*basev0.Configuration{declaredSecretConfiguration()}}
	manager, err := configurations.NewManager(ctx, &resources.Workspace{})
	require.NoError(t, err)
	manager.WithLoader(loader)
	manager.WithSecretResolver(snapshotSecretResolver{})

	require.NoError(t, manager.Load(ctx, &resources.Environment{Name: "staging"}))

	resolved := loader.Configurations()[0]
	require.Equal(t, snapshotSecretPlaceholder, resolved.GetInfos()[0].GetConfigurationValues()[0].GetValue())

	sanitized, _, references, err := promotableDeploymentConfigurations(resolved, nil, "secret-accounts")
	require.NoError(t, err)
	require.Empty(t, sanitized.GetInfos())
	require.Len(t, references, 1)
	for _, reference := range references {
		require.Equal(t, "secret-accounts", reference.GetName())
	}
}

// NewFlow itself must wire the value-free behavior for a snapshot render:
// register the resolver (so a reference-only secret loads without a provider)
// and bind the promotable profile (so the placeholder is always stripped). This
// is the wiring the fix lives in, so a regression that drops it fails here.
func TestNewFlowInSnapshotModeIsValueFreeAndPromotable(t *testing.T) {
	flow := newSnapshotWorkspaceFlow(t, SnapshotMode)

	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		flow.world.KubernetesOutputProfile)

	flow.ConfigurationManager.WithLoader(staticConfigurationLoader{
		confs: []*basev0.Configuration{declaredSecretConfiguration()},
	})
	require.NoError(t, flow.ConfigurationManager.Load(context.Background(), flow.world.Env))
}

// Every other mode keeps the real resolution contract: no snapshot resolver and
// no forced profile, so a reference-only secret with no provider still fails.
func TestNewFlowOutsideSnapshotModeDoesNotRegisterTheResolver(t *testing.T) {
	flow := newSnapshotWorkspaceFlow(t, RunMode)

	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_UNSPECIFIED,
		flow.world.KubernetesOutputProfile)

	flow.ConfigurationManager.WithLoader(staticConfigurationLoader{
		confs: []*basev0.Configuration{declaredSecretConfiguration()},
	})
	require.ErrorContains(t,
		flow.ConfigurationManager.Load(context.Background(), flow.world.Env),
		"requires a backend that is not configured")
}
