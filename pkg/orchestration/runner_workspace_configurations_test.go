package orchestration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/network"
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

// loadedWorkspaceWorld is a World over one static loader, bound the way NewFlow
// binds one: the manager and the composition-root group source come from the
// same loader. Binding only the manager is not a lighter version of this — the
// resolution plans producer discovery against the root's group names, so a World
// that cannot name a root group refuses it (requireKnownRootGroup) rather than
// resolve its references against nothing.
// The tests built on this helper are the RUN path's pre-existing contract,
// carried forward onto the shared resolution unchanged: the union, the
// deduplication, the profile exclusions, the address family a consumer's
// mappings give a reference. They are expected to pass against the behaviour
// before this PR as well — that is what "carried forward unchanged" means — so
// they are regression guards, not evidence that this PR changes anything. The
// differential evidence is the root-only fixture
// (workspace_configurations_parity_test.go) and the refusal tests. Each of
// these is still killed by the targeted mutation of the behaviour it names:
// removing the union deduplication fails TestWorkspaceConfigurationsForDeduplicatesOverlap,
// and so on.
func loadedWorkspaceWorld(t *testing.T, loader staticWorkspaceLoader) *World {
	t.Helper()
	return &World{
		ConfigurationManager:  loadedWorkspaceManager(t, loader),
		compositionRootGroups: loader.CompositionRootWorkspaceConfigurationNames,
		// Also from the same loader: the configurations as loaded, pre
		// interpolation. Without them the resolution can neither validate a
		// reference nor notice a dropped value, so it refuses to resolve a
		// group carrying one at all rather than do either silently.
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return workspaceConfigurationInfos(loader.Configurations())
		},
	}
}

// A composed service reads the composition root's workspace configurations even
// when it declares none of them as dependencies: the root set is unioned into
// every service.
func TestWorkspaceConfigurationsForInjectsCompositionRootSet(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
		},
		rootConfigs: []string{"work-context"},
	})

	confs, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"work-context"}, workspaceConfigurationNames(confs))
}

// The union of declared dependencies and the composition-root set is returned.
func TestWorkspaceConfigurationsForUnionsDeclaredAndRoot(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
		},
		rootConfigs: []string{"work-context"},
	})

	confs, err := world.workspaceConfigurationsFor(context.Background(),
		&resources.Service{WorkspaceConfigurationDependencies: []string{"db"}}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"db", "work-context"}, workspaceConfigurationNames(confs))
}

// A name that is both a declared dependency and a composition-root
// configuration (e.g. the root service declaring its own workspace config) is
// emitted once, not duplicated.
func TestWorkspaceConfigurationsForDeduplicatesOverlap(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
		},
		rootConfigs: []string{"work-context"},
	})

	confs, err := world.workspaceConfigurationsFor(context.Background(),
		&resources.Service{WorkspaceConfigurationDependencies: []string{"db", "work-context"}}, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"db", "work-context"}, workspaceConfigurationNames(confs))
}

// Profile-excluded workspace configurations are dropped from both the declared
// and the composition-root sets.
func TestWorkspaceConfigurationsForExcludesProfiledConfigurations(t *testing.T) {
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("db", "url", "postgres://db"),
			workspaceConfiguration("work-context", "authority-jwks-url", "https://jwks"),
			workspaceConfiguration("managed-auth", "token", "secret"),
		},
		rootConfigs: []string{"work-context", "managed-auth"},
	})
	world.excludedWorkspaceConfigurations = map[string]bool{"managed-auth": true}

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
	world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{
			workspaceConfiguration("platform", "gateway-endpoint", "http://${endpoint:saas/auth-gateway/rest}"),
		},
	})
	// A real workspace holding the producer, because a resolution that carries a
	// reference and has no workspace to check it against is refused now: the
	// lookup is what answers "does this producer exist and may this consumer see
	// that endpoint", and a World without one used to resolve the reference
	// unchecked. Binding it also means this test exercises the lookup rather
	// than skipping it.
	world.Workspace = writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: acme\nlayout: modules\nmodules:\n    - name: saas\n    - name: platform\n",
		"modules/saas/module.codefly.yaml": "kind: module\nname: saas\nproject: acme\n" +
			"domain: github.com/codefly-ai/acme/saas\nservices:\n    - name: auth-gateway\n",
		"modules/saas/services/auth-gateway/service.codefly.yaml": "kind: service\nname: auth-gateway\nversion: 0.0.0\nmodule: saas\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: rest\n      api: rest\n      visibility: public\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: acme\n" +
			"domain: github.com/codefly-ai/acme/platform\nservices:\n    - name: relay\n",
		"modules/platform/services/relay/service.codefly.yaml": "kind: service\nname: relay\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
	})
	// The consumer is loaded from that workspace rather than hand-built: core's
	// reference check reads its identity, and a service with no module has none.
	service, err := loadService(context.Background(), t, world.Workspace, "platform", "relay")
	require.NoError(t, err)
	service.WorkspaceConfigurationDependencies = []string{"platform"}
	mappings := []*basev0.NetworkMapping{{
		Endpoint: &basev0.Endpoint{Module: "saas", Service: "auth-gateway", Name: "rest", Api: "rest"},
		Instances: []*basev0.NetworkInstance{
			{Address: "localhost:38342", Access: resources.NewNativeNetworkAccess()},
			{Address: "auth-gateway.platform-acme-saas.svc.cluster.local:8080", Access: resources.NewContainerNetworkAccess()},
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
	require.Equal(t, "http://auth-gateway.platform-acme-saas.svc.cluster.local:8080", value)

	// A producer of the run with no address for this consumer is a fault the
	// read reports, naming the key and the producer, never an omitted key.
	world.setRunProducers([]string{"saas/auth-gateway"}, nil)
	_, err = world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a reference to a producer of the run with no address fails, never an omitted key")
	require.Contains(t, err.Error(), "platform/gateway-endpoint")
	// Core v0.11.0 names what the manifest says — the endpoint — and locates the
	// reference by position. The producer came from the value's tokens, so it is
	// no longer echoed.
	require.Contains(t, err.Error(), `names endpoint "rest"`)
	require.Contains(t, err.Error(), "reference 1 of 1")
	require.Contains(t, err.Error(), "platform/gateway-endpoint")

	// A producer the run does not contain is not for this run: the consumer
	// does not receive the key. Whether the workspace can resolve it at all is
	// the plan-time check's question (TestPlanConfigurationReferences).
	world.setRunProducers(nil, nil)
	dropped, err := world.workspaceConfigurationsFor(context.Background(), service, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.False(t, configurationsCarryKey(dropped, "platform", "gateway-endpoint"),
		"a reference to a producer outside the run is dropped for the consumer")
}

// configurationsCarryKey reports whether any configuration carries name/key at
// all. resources.GetConfigurationValue answers "" and no error for an absent
// key, which cannot tell a dropped value from an empty one.
func configurationsCarryKey(confs []*basev0.Configuration, name, key string) bool {
	for _, conf := range confs {
		for _, info := range conf.GetInfos() {
			if info.GetName() != name {
				continue
			}
			for _, value := range info.GetConfigurationValues() {
				if value.GetKey() == key {
					return true
				}
			}
		}
	}
	return false
}

// A workspace configuration the consumer declares may name a producer the
// consumer does not depend on — the composition root binding the host by its
// own name for it. Before the producer initializes, the reference resolves to
// the mappings its Init proposes, derived from the endpoints it recorded at
// Load.
//
// A producer that EXISTS but is outside this run is not for this run: the read
// drops it for the consumer, because no local address exists for a service the
// run does not contain. That is `platform/warden` below — a real service, real
// endpoint, simply not in this run set.
//
// A producer that is not a service of the workspace at all is a different thing
// and is REFUSED, by core's verdict, wherever the value resolves. It used to be
// dropped here and refused only at the plan gate, which a dynamic review showed
// to be a fail-open: a typo supplied through an override reached the resolution
// while the gate was still reading the pre-override configurations, so nothing
// refused it anywhere. The two cases are asserted separately below.
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

	loader := staticWorkspaceLoader{
		confs: []*basev0.Configuration{{
			Origin: resources.ConfigurationWorkspace,
			Infos: []*basev0.ConfigurationInformation{{
				Name: "platform",
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "accounts-endpoint", Value: "${endpoint:saas/accounts/connect}"},
					{Key: "elsewhere", Value: "${endpoint:platform/warden/rest}"},
				},
			}},
		}},
	}
	manager := loadedWorkspaceManager(t, loader)
	localNetwork, err := network.NewRuntimeManager(ctx, manager)
	require.NoError(t, err)
	world := &World{
		Env: env, Workspace: workspace,
		ConfigurationManager: manager, SharedState: sharedState, Dependencies: dependencies,
		LocalNetworkManager: localNetwork,
		runtimeContextFor:   func(*resources.Service) string { return resources.RuntimeContextNative },
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return workspaceConfigurationInfos(loader.Configurations())
		},
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

	world.setRunProducers([]string{"saas/accounts"}, consumer)
	mixed, err := world.workspaceConfigurationsFor(ctx, consumer, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err, "a producer outside the run never fails the read")
	require.Len(t, mixed, 1)
	resolved, err := resources.GetConfigurationValue(ctx, mixed[0], "platform", "accounts-endpoint")
	require.NoError(t, err)
	require.Regexp(t, `^http://localhost:\d+$`, resolved)
	require.False(t, configurationsCarryKey(mixed, "platform", "elsewhere"),
		"a producer that exists but is outside the run is dropped for the consumer")

	// And the other case, which is not a drop: a producer the workspace does not
	// have is refused by core's own verdict rather than left to another gate.
	absentLoader := staticWorkspaceLoader{
		confs: []*basev0.Configuration{workspaceConfiguration("platform", "nowhere", "${endpoint:absent/service/http}")},
	}
	// Built fresh rather than copied from `world`: a World carries a sync.Once
	// for its memoized producer lookup, and copying one is a `go vet` copylocks
	// diagnostic — and would share the memoized verdict of a different
	// workspace.
	refusing := &World{
		Env: env, Workspace: workspace,
		ConfigurationManager: loadedWorkspaceManager(t, absentLoader),
		SharedState:          sharedState,
		Dependencies:         dependencies,
		LocalNetworkManager:  localNetwork,
		runtimeContextFor:    func(*resources.Service) string { return resources.RuntimeContextNative },
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return workspaceConfigurationInfos(absentLoader.Configurations())
		},
	}
	refusing.setRunProducers([]string{"saas/accounts"}, consumer)
	_, err = refusing.workspaceConfigurationsFor(ctx, consumer, nil, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a producer that is not a service of the workspace must be refused, not dropped")
	require.Contains(t, err.Error(), "not a service of this workspace")

	manager = loadedWorkspaceManager(t, staticWorkspaceLoader{
		confs: []*basev0.Configuration{workspaceConfiguration("platform", "accounts-endpoint", "${endpoint:saas/accounts/connect}")},
	})
	world.ConfigurationManager = manager
	confs, err := world.workspaceConfigurationsFor(ctx, consumer, nil, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.Len(t, confs, 1)
	derived, err := resources.GetConfigurationValue(ctx, confs[0], "platform", "accounts-endpoint")
	require.NoError(t, err)
	require.Regexp(t, `^http://localhost:\d+$`, derived)

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

	provided, _, err := WorkspaceConfigurationsForChecking(ctx, workspace, env)
	require.NoError(t, err)
	option := configurationReferenceOptionFrom(provided)
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

// A consumer declaring a producer `kind: external` and reaching it through a
// workspace group the composition root writes: the external edge orders
// nothing, so it must not stand in for the reference. The run starts the
// producer first, and the consumer's read of the group resolves its address
// even before the producer has recorded any mapping — never an omitted key.
//
// "Before it has recorded a mapping" is not "before it is in the run": the
// address is derived from the endpoints the producer recorded at Load, which is
// what makes it the address that producer will serve. A producer that never
// loaded is not in the run and has no address to give, and a run with ephemeral
// ports has none to give either — both must say so rather than hand back a free
// port nothing is listening on.
func TestConfigurationReferencesToAProducerDeclaredExternal(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/configuration-references")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	plain, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	plainRun, err := plain.ForStage(resources.StageRun)
	require.NoError(t, err)
	order, err := plainRun.OrderTo(ctx, "platform/relay")
	require.NoError(t, err)
	require.Empty(t, order, "the external declaration alone orders nothing")

	provided, _, err := WorkspaceConfigurationsForChecking(ctx, workspace, env)
	require.NoError(t, err)
	option := configurationReferenceOptionFrom(provided)
	require.NotNil(t, option)
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace, option)
	require.NoError(t, err)
	run, err := dependencies.ForStage(resources.StageRun)
	require.NoError(t, err)
	order, err = run.OrderTo(ctx, "platform/relay")
	require.NoError(t, err)
	require.Equal(t, []architecture.Service{{Unique: "saas/accounts"}}, order)

	sharedState, err := NewStateManager(ctx, nil, dependencies)
	require.NoError(t, err)
	loader := staticWorkspaceLoader{
		confs: []*basev0.Configuration{workspaceConfiguration("platform", "accounts-endpoint", "${endpoint:saas/accounts/connect}")},
	}
	manager := loadedWorkspaceManager(t, loader)
	localNetwork, err := network.NewRuntimeManager(ctx, manager)
	require.NoError(t, err)
	world := &World{
		Env: env, Workspace: workspace,
		ConfigurationManager: manager, SharedState: sharedState, Dependencies: dependencies,
		LocalNetworkManager: localNetwork,
		runtimeContextFor:   func(*resources.Service) string { return resources.RuntimeContextNative },
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return workspaceConfigurationInfos(loader.Configurations())
		},
	}
	platform, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	relay, err := platform.LoadServiceFromName(ctx, "relay")
	require.NoError(t, err)

	dependencyMappings, err := sharedState.GetDependenciesNetworkMappings(ctx, relay)
	require.NoError(t, err)
	require.Empty(t, dependencyMappings, "the external producer has recorded nothing yet")

	// The producer is not in the run: no endpoints recorded at Load, so no
	// address exists for it and the read fails naming the key and the producer.
	// Deriving one from its manifest would give the consumer a free local port
	// no service is behind.
	world.setRunProducers([]string{"saas/accounts"}, relay)
	_, err = world.workspaceConfigurationsFor(ctx, relay, dependencyMappings, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a producer that never loaded has no address, so the read fails")
	require.Contains(t, err.Error(), "platform/accounts-endpoint")
	require.Contains(t, err.Error(), `names endpoint "connect"`)
	require.Contains(t, err.Error(), "platform/accounts-endpoint")

	// The run orders the producer first, so it has loaded and recorded its
	// endpoints by the time the consumer reads the group. Its address is then
	// the one its own Init proposes, a function of its identity.
	accounts, err := loadService(ctx, t, workspace, "saas", "accounts")
	require.NoError(t, err)
	accountsEndpoints, err := accounts.LoadEndpoints(ctx)
	require.NoError(t, err)
	accountsIdentity, err := accounts.Identity()
	require.NoError(t, err)
	require.NoError(t, sharedState.RecordEndpoints(ctx, accountsIdentity, accountsEndpoints))

	confs, err := world.workspaceConfigurationsFor(ctx, relay, dependencyMappings, resources.NewNativeNetworkAccess())
	require.NoError(t, err)
	require.Len(t, confs, 1)
	value, err := resources.GetConfigurationValue(ctx, confs[0], "platform", "accounts-endpoint")
	require.NoError(t, err)
	require.Regexp(t, `^http://localhost:\d+$`, value)

	// With ephemeral ports the proposal is not the address: every call takes a
	// fresh port from the kernel, so it must refuse rather than hand out one the
	// producer will not be listening on.
	world.temporaryPorts = true
	localNetwork.WithTemporaryPorts()
	_, err = world.workspaceConfigurationsFor(ctx, relay, dependencyMappings, resources.NewNativeNetworkAccess())
	require.Error(t, err, "an ephemeral port cannot be derived before the producer initializes")
	require.Contains(t, err.Error(), "--temporary-ports")
	require.Contains(t, err.Error(), "saas/accounts")
}

// loadService loads one service of a workspace by module and name.
func loadService(ctx context.Context, t *testing.T, workspace *resources.Workspace, module, service string) (*resources.Service, error) {
	t.Helper()
	mod, err := workspace.LoadModuleFromName(ctx, module)
	if err != nil {
		return nil, err
	}
	return mod.LoadServiceFromName(ctx, service)
}

// copyConfigurationReferencesWorkspace copies testdata/configuration-references
// and replaces its local `platform` group with platformEnv.
func copyConfigurationReferencesWorkspace(t *testing.T, platformEnv string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS("testdata/configuration-references")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "configurations", "local", "platform.env"), []byte(platformEnv), 0o644))
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

// A run refuses a configuration error when its flow is planned, before any
// service of the run set is created or started, and lists every unresolved
// reference of every service in the run at once.
//
// `platform` is the composition root's own group, so EVERY service of the run
// receives it — the consumer that declares it and the producer that does not.
// Both are listed below for that reason, and that is the behaviour under test as
// much as the refusal is: this gate runs inside InitManagers, and until the
// group names were sourced from a read rather than from the loader (which Load
// populates afterwards) it saw no root group at all and checked declared groups
// only. If this test ever reports one consumer again, the gate has gone back to
// doing nothing for root groups.
func TestRunRefusesUnresolvedConfigurationReferencesBeforeStartingAnything(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := copyConfigurationReferencesWorkspace(t,
		"accounts-endpoint=${endpoint:saas/accounts/connect}\n"+
			"documents-endpoint=${endpoint:documents/store/grpc}\n"+
			"accounts-admin=${endpoint:saas/accounts/admin}\n")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	platform, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	relay, err := platform.LoadServiceFromName(ctx, "relay")
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })
	err = flow.InitManagers(ctx)
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
	// A finding is located by consumer, key and POSITION: core v0.11.0 stopped
	// carrying the reference's text, because a reference is text from a value
	// and a value may be a secret. Position plus the key is enough to find it.
	var got []string
	for _, reference := range unresolved.References {
		got = append(got, fmt.Sprintf("%s %s #%d", reference.Consumer, reference.Key, reference.Position))
	}
	slices.Sort(got)
	require.Equal(t, []string{
		"platform/relay accounts-admin #1",
		"platform/relay documents-endpoint #1",
		"saas/accounts accounts-admin #1",
		"saas/accounts documents-endpoint #1",
	}, got, "a root group's faults are reported for every service that receives it")
	require.Empty(t, flow.hub.managers, "no service of the run set was created")
}

// The same plan check, for a plan with no flow yet, and a resolvable plan
// passes it.
func TestPlanConfigurationReferences(t *testing.T) {
	ctx := context.Background()
	broken := copyConfigurationReferencesWorkspace(t, "documents-endpoint=${endpoint:documents/store/grpc}\n")
	env, err := SelectEnvironment(broken, LocalEnvironmentName)
	require.NoError(t, err)
	warden, err := broken.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	root, err := warden.LoadServiceFromName(ctx, "warden")
	require.NoError(t, err)
	err = PlanConfigurationReferences(ctx, broken, env, []*resources.Service{root}, false)
	// The finding's shape in core v0.11.0: consumer, group/key, position and
	// reason — no reference text, no producer taken from it.
	require.ErrorContains(t, err, "platform/warden: platform/documents-endpoint, reference 1: the producer is not a service of this workspace")

	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/configuration-references")
	require.NoError(t, err)
	env, err = SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	root, err = module.LoadServiceFromName(ctx, "warden")
	require.NoError(t, err)
	require.NoError(t, PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{root}, false))
}

// Excluding a producer from the run (--exclude-dependency, a run profile) takes
// it out of the graph, so core cannot tell it from a service of another
// workspace and reports "not a service of this plan" — which sends the reader
// hunting for a missing composition instead of at their own exclusion. The
// refusal must name the exclusion and the way out.
func TestRunNamesTheExclusionThatLeftAReferenceWithNoProducer(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := copyConfigurationReferencesWorkspace(t, "accounts-endpoint=${endpoint:saas/accounts/connect}\n")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	platform, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	relay, err := platform.LoadServiceFromName(ctx, "relay")
	require.NoError(t, err)

	resolved, err := workspace.ResolveRunProfile(ctx, "", resources.RunProfile{ExcludeDependencies: []string{"saas/accounts"}})
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })
	require.NoError(t, flow.WithRunProfile(resolved))

	err = flow.InitManagers(ctx)
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
	require.Len(t, unresolved.References, 1)
	// The producer is named in the reason this package writes — derived from the
	// value it already holds, by the position core reported — so the operator
	// still learns which excluded service caused it.
	require.Contains(t, unresolved.References[0].Reason, "saas/accounts")
	require.Contains(t, unresolved.References[0].Reason, "excluded from this run")
	require.Contains(t, unresolved.References[0].Reason, `exclude the "platform" workspace configuration too`)
	require.NotContains(t, unresolved.References[0].Reason, "not a service of this plan")

	// Excluding the group as the refusal instructs gets past the check.
	flow, err = NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })
	require.NoError(t, flow.WithRunProfile(resources.RunProfile{
		ExcludeDependencies:            []string{"saas/accounts"},
		ExcludeWorkspaceConfigurations: []string{"platform"},
	}))
	err = flow.InitManagers(ctx)
	require.False(t, errors.As(err, &unresolved), "excluding the group too clears the reference check, got %v", err)
}

// A service bound to a remote environment resolves no workspace configuration
// at all (Runner.InitRemote only sets up networking), so a reference in a group
// it declares is never read and must not refuse the local run.
func TestRemoteBoundServiceIsNotCheckedForConfigurationReferences(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := copyConfigurationReferencesWorkspace(t, "documents-endpoint=${endpoint:documents/store/grpc}\n")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	platform, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	relay, err := platform.LoadServiceFromName(ctx, "relay")
	require.NoError(t, err)

	// Locally, relay's unresolvable reference refuses the run.
	local, err := NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Stop() })
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(local.InitManagers(ctx), &unresolved))

	// Bound to a remote environment, the same reference is not its run's to
	// resolve, so the check must let the plan through.
	remote, err := NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Stop() })
	remote.WithRemotes([]*Remote{{
		ServiceWithModule: &resources.ServiceWithModule{Name: "relay", Module: "platform"},
		Environment:       env,
	}})
	err = remote.InitManagers(ctx)
	require.False(t, errors.As(err, &unresolved), "a remote-bound service resolves no group, got %v", err)
}

// referenceValidityFlow builds the real Flow for the two-module composition
// referenceValidityWorkspace describes: the origin is payments/worker, which
// declares no workspace configuration group at all, so everything it receives is
// the composition root's. That is what makes these tests about the gate's
// extension rather than about its declared-group half.
func referenceValidityFlow(t *testing.T, workspace *resources.Workspace, options ...FlowOption) *Flow {
	t.Helper()
	ctx := context.Background()
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	payments, err := workspace.LoadModuleFromName(ctx, "payments")
	require.NoError(t, err)
	worker, err := payments.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)
	require.Empty(t, worker.WorkspaceConfigurationDependencies,
		"the consumer must declare nothing for this to be about the effective set")

	flow, err := NewFlow(ctx, workspace, payments, worker, env, RunMode, options...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })
	return flow
}

// The flow's plan gate sees a composition-root group's VISIBILITY violation,
// and sees it at plan time rather than mid-run.
//
// It is the half of the gate extension nothing covered: the gate runs inside
// InitManagers, and sourcing its group names from the loader returned nothing
// there, because Load populates them afterwards. So a root group's reference
// was checked for nobody until each service reached its own Init. This drives
// the real Flow.
//
// It is a cross-module case on an endpoint that EXISTS. An earlier revision of
// this test referenced an endpoint the producer did not declare, from a consumer
// in the producer's own module — so it was a missing-endpoint test wearing a
// visibility name, and it would have passed with the visibility rule removed
// altogether. resources.ValidateEndpointVisibility returns nil within one
// module, which is exactly why the consumer here is in the other one.
func TestTheFlowPlanGateRefusesARootGroupsVisibilityViolation(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	for _, visibility := range []string{"private", "internal"} {
		t.Run(visibility, func(t *testing.T) {
			flow := referenceValidityFlow(t,
				referenceValidityWorkspace(t, "platform/authority/admin", visibility))

			err := flow.InitManagers(context.Background())
			var unresolved *configurations.UnresolvedReferencesError
			require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
			require.NotEmpty(t, unresolved.References)
			// Core's reason names what the MANIFEST says — the producing
			// service and endpoint — rather than the reference's text.
			require.Contains(t, unresolved.References[0].Reason, "authority")
			require.Contains(t, unresolved.References[0].Reason, "payments",
				"the refusal must name the module that may not see the endpoint")
			// The reason is core's own visibility verdict, asserted so the test
			// cannot pass on a different fault wearing the same name.
			switch visibility {
			case "private":
				require.Contains(t, unresolved.References[0].Reason, "is private to module")
			case "internal":
				require.Contains(t, unresolved.References[0].Reason, "does not permit module")
			}
			require.Empty(t, flow.hub.managers, "no service of the run set was created")
		})
	}
}

// The same group with a visible endpoint passes the gate, so the test above is
// about the boundary and not about root references refusing runs in general.
//
// It is a negative control, and that is a structural limit on what it can
// prove: its assertion is the ABSENCE of a refusal, and the behaviour before
// this PR refused nothing here either, so no rollback of this PR can make it
// fail. The same is true of every "must not refuse" test in this package
// (TestTheFlowPlanGateDoesNotRefuseARootProducerOutsideTheRunClosure,
// TestARunDoesNotRefuseARootValueThatDidNotSurviveResolution,
// TestARunWithTemporaryPortsDropsARootReferenceItCannotPlaceYet). What they
// constrain is the refusal mechanism going too far, so they are killed by the
// targeted mutation that widens it — not by removing it.
func TestTheFlowPlanGateAcceptsAVisibleRootGroupReference(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	flow := referenceValidityFlow(t,
		referenceValidityWorkspace(t, "platform/authority/admin", "public"))

	err := flow.InitManagers(context.Background())
	var unresolved *configurations.UnresolvedReferencesError
	require.False(t, errors.As(err, &unresolved), "a visible root reference must pass the plan gate: %v", err)
}

// A module-closure run whose root group names a producer outside the closure is
// NOT refused — the trap the gate had to avoid while being extended.
//
// The gate's lookup for a declared group is the plan's graph, where an excluded
// producer is absent by design. A root group reaches every service of every run,
// including a closure run (`--module`) whose graph is a slice of the workspace,
// so judging a root group against that slice would report a perfectly real
// producer as "not a service of this workspace" and refuse the run. Root groups
// are therefore judged against the whole workspace: whether a producer is in
// THIS run is not a plan-time question.
//
// The closure is REAL here, and that is the correction this test needed. Its
// earlier shape built none — the flow's graph was the whole workspace, so the
// producer was in it and the trap was never set; the test passed with root
// groups judged against the plan graph, which is the mutation it exists to
// catch. WithRunModuleClosure("payments") gives the flow a graph of the payments
// module alone, and platform/authority is genuinely not in it.
func TestTheFlowPlanGateDoesNotRefuseARootProducerOutsideTheRunClosure(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	flow := referenceValidityFlow(t, workspace, WithRunModuleClosure("payments"))

	// The graph this run was given really does lack the producer: without this
	// the test would be asserting the absence of a refusal that nothing could
	// have produced.
	_, err := flow.world.Dependencies.ServiceFromUnique("platform/authority")
	require.Error(t, err, "the closure must not contain the producer, or the trap is not set")

	err = flow.InitManagers(ctx)
	var unresolved *configurations.UnresolvedReferencesError
	require.False(t, errors.As(err, &unresolved),
		"a legal root reference must not refuse a run just because this run is smaller than the workspace: %v", err)
}

// The flow's plan gate reads the configurations this invocation will RESOLVE,
// overrides included — not the ones on disk.
//
// The two halves of this were separately correct and jointly silent. The gate
// read the workspace configurations with a plain disk read, which cannot see an
// invocation-scoped override, and the resolution tolerated a nonexistent
// producer because "the gate refuses that". So a typo an operator supplied on
// their own command line passed the gate reading a value it did not have, and
// was then dropped by the resolution deferring to that gate — the value simply
// absent, no error anywhere.
//
// PlanConfigurationReferences is pinned for this by
// TestAnInvocationOverrideIsCheckedLikeAnyOtherReference; this pins the FLOW's
// gate, which is the path `codefly run` takes and a different call site. Both
// are needed: restoring the pre-override read in one of them leaves the other
// green.
func TestTheFlowPlanGateChecksAnInvocationOverride(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	// Correct on disk. The override is what breaks it.
	workspace := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{
		{Name: "work-context", Key: "authority-endpoint", Value: "${endpoint:platfrom/authority/admin}"},
	})
	require.NoError(t, err)
	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)

	flow := referenceValidityFlow(t, workspace)
	err = flow.InitManagers(ctx)
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved),
		"a typo supplied through the override carrier must not pass the flow's plan gate, got %v", err)
	// The typo itself is NOT echoed: core v0.11.0 carries no text from the
	// value, and a mistyped producer is value text like any other. What the
	// operator gets is the group, the key and which reference of that value it
	// was — enough to find it in their own file — plus the reason.
	require.Equal(t, "work-context", unresolved.References[0].Group)
	require.Equal(t, 1, unresolved.References[0].Position)
	require.Contains(t, unresolved.References[0].Reason, "not a service of this workspace")
	require.NotContains(t, unresolved.References[0].Reason, "platfrom")
	require.Empty(t, flow.hub.managers, "no service of the run set was created")
}

// A run profile that excludes a composition-root group excludes it from the
// plan gate's ROOT half too.
//
// The exclusion is the one asymmetry this package allows: a profile's
// `exclude-workspace-configurations` trims the run and never a render, so a
// profile can leave the run with fewer groups than the render. A group no
// service of the run receives must not refuse that run.
//
// Core's check reads the exclusions off the profile, and the gate's root half is
// a restatement of each consumer carrying the groups it did not declare — handed
// to that same check, with that same profile. So the exclusion holds for the
// root half too, and `consumersWithRootGroupsOnly`'s own exclusion filter only
// decides whether the second check is run at all. That is why removing it
// changes no verdict: the layer-5 review reported that mutation as surviving,
// and it survives because it is observationally equivalent, not because the
// behaviour is unpinned. This test pins the behaviour, end to end, through the
// real flow.
//
// (A composition-root group that NO service of the workspace declares cannot be
// excluded by a profile at all: core builds its inventory of known group names
// from service declarations, so `ResolveRunProfile` calls such a name unknown.
// That is a core-side gap, named in the PR body, and the reason this test uses a
// group one service declares and another receives run-wide.)
func TestARunProfileExclusionAlsoExcludesARootGroupFromThePlanGate(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	// A producer no module of this workspace has, in the `platform` group:
	// platform/relay declares that group, saas/accounts does not and receives it
	// from the composition root.
	//
	// The resolvable reference beside it is what puts saas/accounts in the run
	// set: a configuration reference is an ordering edge
	// (WithConfigurationReferences), and relay's own dependency on accounts is
	// `external`, which codefly never starts.
	workspace := copyConfigurationReferencesWorkspace(t,
		"accounts-endpoint=${endpoint:saas/accounts/connect}\n"+
			"documents-endpoint=${endpoint:documents/store/grpc}\n")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	platform, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	relay, err := platform.LoadServiceFromName(ctx, "relay")
	require.NoError(t, err)

	refusing, err := NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = refusing.Stop() })
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(refusing.InitManagers(ctx), &unresolved),
		"the group's unresolvable reference must refuse the run while the run receives the group")
	var consumers []string
	for _, reference := range unresolved.References {
		consumers = append(consumers, reference.Consumer)
	}
	require.Contains(t, consumers, "saas/accounts",
		"the root half must be what covers a consumer that declares nothing, or this test excludes nothing")

	excluded, err := workspace.ResolveRunProfile(ctx, "", resources.RunProfile{
		ExcludeWorkspaceConfigurations: []string{"platform"},
	})
	require.NoError(t, err)
	passing, err := NewFlow(ctx, workspace, platform, relay, env, RunMode)
	require.NoError(t, err)
	t.Cleanup(func() { _ = passing.Stop() })
	require.NoError(t, passing.WithRunProfile(excluded))
	err = passing.InitManagers(ctx)
	require.False(t, errors.As(err, &unresolved),
		"a group this run does not receive must not refuse it: %v", err)
}
