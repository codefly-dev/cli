package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/agents/contract"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// exposedValues reads back what the agent process exported from its Init, keyed
// by name.
func exposedValues(ctx context.Context, t *testing.T, world *World) map[string]string {
	t.Helper()
	shared, err := world.ConfigurationManager.GetSharedServiceConfiguration(ctx, "web/gateway")
	require.NoError(t, err)
	require.Len(t, shared, 1)
	values := map[string]string{}
	for _, info := range shared[0].GetInfos() {
		for _, value := range info.GetConfigurationValues() {
			values[value.GetKey()] = value.GetValue()
		}
	}
	return values
}

func TestInitDeliversAcceptedDependencyAddressesWithoutStartingConsumer(t *testing.T) {
	ctx := context.Background()
	producer, world := gatewayRunner(t, startRealAgent(t, realAgentBind))
	_, err := producer.Init(ctx)
	require.NoError(t, err)
	service, err := world.Dependencies.ServiceFromUnique("web/frontend")
	require.NoError(t, err)
	identity, err := service.Identity()
	require.NoError(t, err)
	instance := &services.Instance{
		Workspace: world.Workspace, Module: producer.instance.Module, Service: service, Identity: identity,
		Info: &agentv0.AgentInformation{},
	}
	instance.Runtime = &services.RuntimeInstance{Instance: instance, Runtime: startRealAgent(t, realAgentInputs)}
	consumer, err := NewRunner(ctx, instance, world)
	require.NoError(t, err)
	consumer.runtimeContext = resources.RuntimeContextNative
	consumer.endpoints, err = service.LoadEndpoints(ctx)
	require.NoError(t, err)
	consumer.WithTestRequest(&runtimev0.TestRequest{})
	_, err = consumer.Init(ctx)
	require.ErrorContains(t, err, contract.RuntimeInitDependencyMappings)
	instance.Info.Contract = &agentv0.AgentContract{Capabilities: []string{contract.RuntimeInitDependencyMappings}}
	_, err = consumer.Init(ctx)
	require.NoError(t, err)
	configuration, err := world.ConfigurationManager.GetSharedServiceConfiguration(ctx, "web/frontend")
	require.NoError(t, err)
	require.Len(t, configuration, 1)
	observed := map[string]string{}
	for _, info := range configuration[0].GetInfos() {
		for _, value := range info.GetConfigurationValues() {
			observed[value.GetKey()] = value.GetValue()
		}
	}
	for _, mapping := range producer.networkMappings {
		endpoint := mapping.GetEndpoint()
		if !service.ServiceDependencies[0].ConsumesEndpoint(endpoint.GetName(), endpoint.GetApi()) {
			continue
		}
		require.Equal(t, nativeAddressFor(t, producer.networkMappings, endpoint.GetName()), observed[resources.EndpointDestination(endpoint)])
	}
}

// A service under test is not always started: a START_DEPENDENCIES policy
// replaces its Start with a sequencing barrier and NONE skips Start entirely,
// so inputs carried only by StartRequest reach every dependency and never the
// service the suite is about. Init is the one call that always arrives, and
// both inputs are asserted here across a real agent process because the defect
// was precisely that they were put on the wrong request.
func TestInitCarriesTheFixtureAndOverridesToAServiceThatIsNeverStarted(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentInputs))
	runner.WithFixture("dev-admin")
	runner.WithOverrides(map[string]string{"CODEFLY__API_CONSUMES": "documents:wiki/documents/api"})

	_, err := runner.Init(ctx)
	require.NoError(t, err)

	observed := exposedValues(ctx, t, world)
	require.Equal(t, "dev-admin", observed["fixture"],
		"the agent's Init never saw the fixture the invocation selected")
	require.Equal(t, "documents:wiki/documents/api", observed["CODEFLY__API_CONSUMES"],
		"the agent's Init never saw the process overrides a solution entry federates with")
}

// An invocation that selects neither must not stamp empty values on the
// service: an agent takes the first non-empty of the Init and Start values, and
// an empty string delivered as if it were a selection is not the same as none.
func TestInitCarriesNoFixtureOrOverridesWhenTheInvocationSelectsNone(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentInputs))

	_, err := runner.Init(ctx)
	require.NoError(t, err)

	observed := exposedValues(ctx, t, world)
	require.Equal(t, "", observed["fixture"])
	require.NotContains(t, observed, "CODEFLY__API_CONSUMES")
}
