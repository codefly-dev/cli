package orchestration

import (
	"context"
	"testing"

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
