package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// --exclude-root never loads the root agent, so services.RuntimeInstance.Start
// — the narrowing every running service goes through — never runs for it. This
// file is then the ONLY carrier of its dependencies' addresses. It must not
// carry one the producer keeps private: nothing downstream would filter it.
func TestExcludedRootOutputEnvOmitsForbiddenDependencyEndpoint(t *testing.T) {
	ctx := context.Background()
	flow, outputEnv := excludedRootVisibilityFlow(t, ctx)

	require.NoError(t, flow.exportExcludedOriginEnvironment(ctx))

	body, err := os.ReadFile(outputEnv)
	require.NoError(t, err)
	text := string(body)
	require.Contains(t, text, "CODEFLY__ENDPOINT__SAAS__ACCOUNTS__CONNECT__REST=",
		"the public dependency endpoint must still reach the excluded-root environment")
	require.NotContains(t, text, "CODEFLY__ENDPOINT__SAAS__ACCOUNTS__INTERNAL__REST=",
		"an endpoint private to the producer's module reached the excluded-root environment:\n"+text)
}

// excludedRootVisibilityFlow wires the excluded-root exporter for
// testdata/excluded-root-visibility: platform/warden consumes ALL of
// saas/accounts, which publishes one public and one private endpoint.
func excludedRootVisibilityFlow(t *testing.T, ctx context.Context) (*Flow, string) {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/excluded-root-visibility")
	require.NoError(t, err)
	producerModule, err := workspace.LoadModuleFromName(ctx, "saas")
	require.NoError(t, err)
	producer, err := producerModule.LoadServiceFromName(ctx, "accounts")
	require.NoError(t, err)
	consumerModule, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	consumer, err := consumerModule.LoadServiceFromName(ctx, "warden")
	require.NoError(t, err)

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	configurationManager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, configurationManager.Load(ctx, env.Runtime()))
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, configurationManager, dependencies)
	require.NoError(t, err)

	// The producer's endpoints as the dependency graph surfaces them, carrying
	// the visibility its manifest declares.
	endpoints, err := producer.LoadEndpoints(ctx)
	require.NoError(t, err)
	require.Len(t, endpoints, 2)
	mappings := make([]*basev0.NetworkMapping, 0, len(endpoints))
	for _, endpoint := range endpoints {
		mappings = append(mappings, &basev0.NetworkMapping{
			Endpoint: endpoint,
			Instances: []*basev0.NetworkInstance{{
				Address: "http://localhost:10650",
				Access:  resources.NewNativeNetworkAccess(),
			}},
		})
	}
	require.NoError(t, sharedState.RecordNetworkMappings(ctx, producer, mappings))

	outputEnv := filepath.Join(t.TempDir(), "runtime.env")
	flow := &Flow{
		originService:        consumer,
		runtimeContext:       resources.RuntimeContextNative,
		fixture:              "codefly",
		ConfigurationManager: configurationManager,
		SharedState:          sharedState,
		world: &World{
			Env:                  env,
			Workspace:            workspace,
			Dependencies:         dependencies,
			SharedState:          sharedState,
			ConfigurationManager: configurationManager,
		},
	}
	flow.WithOutputEnv(outputEnv)
	flow.WithExcludeRoot(true)
	require.True(t, flow.exportsExcludedOriginEnvironment())
	return flow, outputEnv
}

// A configuration is the producer's live connection, exposed by its agent while
// it runs. A `kind: build` dependency constrains no run stage, so the consumer
// has none to consume: writing it hands the consumer credentials for a service
// it only builds against. This is the credential half of the same leak the
// endpoint narrowing closes, and it must not reach the output environment.
func TestOutputEnvOmitsBuildOnlyDependencyCredentials(t *testing.T) {
	ctx := context.Background()
	flow, outputEnv := excludedRootVisibilityFlow(t, ctx)

	secret := func(origin, name, key, value string) *basev0.Configuration {
		return &basev0.Configuration{
			Origin:         origin,
			RuntimeContext: resources.NewRuntimeContextNative(),
			Infos: []*basev0.ConfigurationInformation{{
				Name: name,
				ConfigurationValues: []*basev0.ConfigurationValue{{
					Key: key, Value: value, Secret: true,
				}},
			}},
		}
	}
	expose := func(module, service string, conf *basev0.Configuration) {
		require.NoError(t, flow.ConfigurationManager.ExposeConfiguration(
			ctx,
			&resources.ServiceIdentity{Module: module, Name: service, Version: "0.0.0"},
			conf,
		))
	}
	expose("saas", "accounts", secret("saas/accounts", "accounts", "connection", "postgres://runtime"))
	expose("saas", "codegen", secret("saas/codegen", "codegen", "connection", "postgres://buildonly"))

	require.NoError(t, flow.exportExcludedOriginEnvironment(ctx))

	body, err := os.ReadFile(outputEnv)
	require.NoError(t, err)
	text := string(body)
	require.Contains(t, text, "CODEFLY__SERVICE_SECRET_CONFIGURATION__SAAS__ACCOUNTS__ACCOUNTS__CONNECTION=postgres://runtime",
		"the run-stage dependency's connection must still reach the environment:\n"+text)
	require.NotContains(t, text, "postgres://buildonly",
		"a build-only dependency's credentials reached the output environment:\n"+text)
}

// Without a consumer module there is no permitted set to compute, so the
// output environment must refuse to write rather than write dependency
// addresses whose visibility nothing checked. Core forwards the agent request
// unnarrowed in this case; this carrier has no second gate behind it.
func TestOutputEnvRefusesToWriteWithoutAConsumerModule(t *testing.T) {
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/excluded-root-visibility")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "platform")
	require.NoError(t, err)
	consumer, err := module.LoadServiceFromName(ctx, "warden")
	require.NoError(t, err)
	identity, err := consumer.Identity()
	require.NoError(t, err)
	identity.Workspace = workspace.Name

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	configurationManager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, configurationManager.Load(ctx, env.Runtime()))
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, configurationManager, dependencies)
	require.NoError(t, err)

	// Module deliberately absent: the field core guards before the identical call.
	instance := &coreservices.Instance{
		Workspace: workspace,
		Service:   consumer,
		Identity:  identity,
	}
	instance.Runtime = &coreservices.RuntimeInstance{Instance: instance}
	outputEnv := filepath.Join(t.TempDir(), "runtime.env")
	runner := &Runner{
		instance:       instance,
		outputEnv:      outputEnv,
		runtimeContext: resources.RuntimeContextNative,
		world: &World{
			Env:                  env,
			Workspace:            workspace,
			Dependencies:         dependencies,
			SharedState:          sharedState,
			ConfigurationManager: configurationManager,
		},
	}

	_, err = runner.Start(ctx)
	require.Error(t, err, "a runner with no module must not write an unchecked output environment")
	require.Contains(t, err.Error(), "has no module")
	require.NoFileExists(t, outputEnv, "nothing may be written when the permitted set cannot be computed")
}
