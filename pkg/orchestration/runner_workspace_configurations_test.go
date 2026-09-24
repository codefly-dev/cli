package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/network"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// staticWorkspaceLoader feeds workspace-origin configurations into a real
// configurations.Manager and, via CompositionRootWorkspaceConfigurationNames,
// declares which of them the composition root itself provides run-wide. It lets
// workspaceConfigurationsFor be exercised against genuine core resolution.
type staticWorkspaceLoader struct {
	confs       []*basev0.Configuration
	rootConfigs []string
}

func (staticWorkspaceLoader) Identity() string { return "static-workspace-loader" }

func (staticWorkspaceLoader) Load(context.Context, *resources.Environment) error { return nil }

func (l staticWorkspaceLoader) Configurations() []*basev0.Configuration { return l.confs }

func (staticWorkspaceLoader) DNS() []*basev0.DNS { return nil }

func (l staticWorkspaceLoader) CompositionRootWorkspaceConfigurationNames() []string {
	return l.rootConfigs
}

func workspaceConfiguration(name, key, value string) *basev0.Configuration {
	return &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name: name,
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: key, Value: value},
			},
		}},
	}
}

func workspaceConfigurationNames(confs []*basev0.Configuration) []string {
	names := make([]string, 0, len(confs))
	for _, conf := range confs {
		for _, info := range conf.Infos {
			names = append(names, info.Name)
		}
	}
	return names
}

func loadedWorkspaceManager(t *testing.T, loader staticWorkspaceLoader) *configurations.Manager {
	t.Helper()
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{"workspace.codefly.yaml": "name: bare\nlayout: flat\n"})
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	manager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	manager.WithLoader(loader)
	require.NoError(t, manager.Load(ctx, env.Runtime()))
	return manager
}

// A composed service reads the composition root's workspace configurations even
// when it declares none of them as dependencies: the root set is unioned into
// every service.
func TestWorkspaceConfigurationsForInjectsCompositionRootSet(t *testing.T) {
	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
		},
		rootConfigs: []string{"work-context"},
	})
	world := &World{ConfigurationManager: manager}

	confs, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"work-context"}, workspaceConfigurationNames(confs))
}

// The union of declared dependencies and the composition-root set is returned.
func TestWorkspaceConfigurationsForUnionsDeclaredAndRoot(t *testing.T) {
	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
		},
		rootConfigs: []string{"work-context"},
	})
	world := &World{ConfigurationManager: manager}

	confs, err := world.workspaceConfigurationsFor(context.Background(),
		&resources.Service{WorkspaceConfigurationDependencies: []string{"db"}}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"db", "work-context"}, workspaceConfigurationNames(confs))
}

// A name that is both a declared dependency and a composition-root
// configuration (e.g. the root service declaring its own workspace config) is
// emitted once, not duplicated.
func TestWorkspaceConfigurationsForDeduplicatesOverlap(t *testing.T) {
	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
		},
		rootConfigs: []string{"work-context"},
	})
	world := &World{ConfigurationManager: manager}

	confs, err := world.workspaceConfigurationsFor(context.Background(),
		&resources.Service{WorkspaceConfigurationDependencies: []string{"db", "work-context"}}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"db", "work-context"}, workspaceConfigurationNames(confs))
}

// Profile-excluded workspace configurations are dropped from both the declared
// and the composition-root sets.
func TestWorkspaceConfigurationsForExcludesProfiledConfigurations(t *testing.T) {
	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
			workspaceConfiguration("managed-auth", "token", "secret"),
		},
		rootConfigs: []string{"work-context", "managed-auth"},
	})
	world := &World{
		ConfigurationManager:            manager,
		excludedWorkspaceConfigurations: map[string]bool{"managed-auth": true},
	}

	confs, err := world.workspaceConfigurationsFor(context.Background(),
		&resources.Service{WorkspaceConfigurationDependencies: []string{"db"}}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"db", "work-context"}, workspaceConfigurationNames(confs))
}

// A workspace configuration value naming ${endpoint:…} resolves against the
// consumer's own dependency mappings, in the address family of its access: the
// same group gives a native consumer its loopback address and a deployed one its
// in-cluster address.
func TestWorkspaceConfigurationsForResolvesEndpointsFromConsumerMappings(t *testing.T) {
	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("platform", "gateway-endpoint", "http://${endpoint:saas/auth-gateway/rest}"),
		},
	})
	world := &World{ConfigurationManager: manager}
	service := &resources.Service{WorkspaceConfigurationDependencies: []string{"platform"}}
	mappings := []*basev0.NetworkMapping{{
		Endpoint: &basev0.Endpoint{Module: "saas", Service: "auth-gateway", Name: "rest", Api: "rest"},
		Instances: []*basev0.NetworkInstance{
			{Address: "localhost:38342", Access: resources.NewNativeNetworkAccess()},
			{Address: "auth-gateway.platform-obin-saas.svc.cluster.local:8080", Access: resources.NewContainerNetworkAccess()},
		},
	}}

	native, err := world.workspaceConfigurationsFor(context.Background(), service, mappings, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	value, err := resources.GetConfigurationValue(context.Background(), native[0], "platform", "gateway-endpoint")
	require.NoError(t, err)
	require.Equal(t, "http://localhost:38342", value)

	deployed, err := world.workspaceConfigurationsFor(context.Background(), service, mappings, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	value, err = resources.GetConfigurationValue(context.Background(), deployed[0], "platform", "gateway-endpoint")
	require.NoError(t, err)
	require.Equal(t, "http://auth-gateway.platform-obin-saas.svc.cluster.local:8080", value)

	undeclared, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	value, err = resources.GetConfigurationValue(context.Background(), undeclared[0], "platform", "gateway-endpoint")
	require.NoError(t, err)
	require.Empty(t, value, "a consumer that does not depend on the endpoint does not receive it")
}

// A workspace configuration the consumer declares may name a producer the
// consumer does not depend on — the composition root binding the host by its
// own name for it. Before the producer initializes, the reference resolves to
// the mappings its Init proposes, derived from the endpoints it recorded at
// Load; a producer outside the run is omitted.
func TestWorkspaceConfigurationsForResolvesReferencedProducersOfTheRun(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/excluded-root-visibility")
	require.NoError(t, err)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, nil, dependencies)
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	manager := loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{{
			Origin: resources.ConfigurationWorkspace,
			Infos: []*basev0.ConfigurationInformation{{
				Name: "platform",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "accounts-endpoint", Value: "${endpoint:saas/accounts/connect}"},
					{Key: "elsewhere", Value: "${endpoint:absent/service/http}"},
				},
			}},
		}},
	})
	localNetwork, err := network.NewRuntimeManager(ctx, manager)
	require.NoError(t, err)
	world := &World{
		Env: env, Workspace: workspace,
		ConfigurationManager: manager, SharedState: sharedState, Dependencies: dependencies,
		LocalNetworkManager: localNetwork,
		runtimeContextFor:   func(*resources.Service) string { return resources.RuntimeContextNative },
	}

	saas, err := workspace.LoadModuleFromName(ctx, "saas")
	require.NoError(t, err)
	accounts, err := saas.LoadServiceFromName(ctx, "accounts")
	require.NoError(t, err)
	accountsIdentity, err := accounts.Identity()
	require.NoError(t, err)
	endpoints, err := accounts.LoadEndpoints(ctx)
	require.NoError(t, err)
	require.NoError(t, sharedState.RecordEndpoints(ctx, accountsIdentity, endpoints))
	consumer, err := saas.LoadServiceFromName(ctx, "codegen")
	require.NoError(t, err)
	require.Empty(t, consumer.ServiceDependencies)
	consumer.WorkspaceConfigurationDependencies = []string{"platform"}

	confs, err := world.workspaceConfigurationsFor(ctx, consumer, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.Len(t, confs, 1)
	derived, err := resources.GetConfigurationValue(ctx, confs[0], "platform", "accounts-endpoint")
	require.NoError(t, err)
	require.Regexp(t, `^http://localhost:\d+$`, derived)
	keys := make([]string, 0, 2)
	for _, value := range confs[0].Infos[0].ConfigurationValues {
		keys = append(keys, value.Key)
	}
	require.Equal(t, []string{"accounts-endpoint"}, keys, "a producer outside the run is omitted for the consumer")

	// Once the producer has initialized, its accepted mappings are what resolves.
	require.NoError(t, sharedState.RecordNetworkMappings(ctx, accounts, []*basev0.NetworkMapping{{
		Endpoint: &basev0.Endpoint{Module: "saas", Service: "accounts", Name: "connect", Api: "rest"},
		Instances: []*basev0.NetworkInstance{{
			Address: "http://localhost:10650",
			Access:  resources.NewNativeNetworkAccess(),
		}},
	}}))
	confs, err = world.workspaceConfigurationsFor(ctx, consumer, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	recorded, err := resources.GetConfigurationValue(ctx, confs[0], "platform", "accounts-endpoint")
	require.NoError(t, err)
	require.Equal(t, "http://localhost:10650", recorded)
}

// A service reaching a producer only through a workspace configuration group
// the composition root writes is ordered after it, as for a declared dependency.
func TestConfigurationReferencesOrderTheRun(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/configuration-references")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	option := configurationReferenceOption(ctx, workspace, env)
	require.NotNil(t, option)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace, option)
	require.NoError(t, err)
	order, err := dependencies.OrderTo(ctx, "platform/warden")
	require.NoError(t, err)
	require.Equal(t, []architecture.Service{{Unique: "saas/accounts"}}, order)

	plain, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	order, err = plain.OrderTo(ctx, "platform/warden")
	require.NoError(t, err)
	require.Empty(t, order)
}
