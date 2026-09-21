package gateway

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestConfigureServiceForwardsOpaqueChangesAndReusesSession(t *testing.T) {
	root := t.TempDir()
	selected := protocoltest.Install(t, "configuration-peer")[0]
	writeCodeUnitFixture(t, root, "mind.yaml", "service: source\nplugin: "+selected+"\n")
	server, err := NewServer(Config{WorkDir: root})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	changes := []*builderv0.ConfigChange{{Path: "opaque.option", Value: "selected-value", Op: builderv0.ConfigChange_SET}}
	configured, err := server.ConfigureService(t.Context(), &gatewayv1.ConfigureServiceRequest{Changes: changes})
	require.NoError(t, err)
	require.Equal(t, builderv0.ConfigureStatus_SUCCESS, configured.GetResponse().GetState().GetState())
	require.Equal(t, "opaque: peer-response\n", configured.GetResponse().GetEffectiveYaml())
	reset := &builderv0.ConfigChange{Path: "opaque.option", Op: builderv0.ConfigChange_UNSET}
	_, err = server.ConfigureService(t.Context(), &gatewayv1.ConfigureServiceRequest{Changes: []*builderv0.ConfigChange{reset}})
	require.NoError(t, err)
	tested, err := server.Test(t.Context(), &gatewayv1.TestRequest{RuntimeRequest: &runtimev0.TestRequest{Target: "opaque-test-selector"}})
	require.NoError(t, err)
	require.Equal(t, runtimev0.TestRunResult_PASSED, tested.GetRuntimeResponse().GetResult().GetState())
	var methods []string
	var pid int
	var configuredCalls int
	for _, call := range protocoltest.Calls(t, root) {
		if call.Method == "Agent.GetAgentInformation" {
			continue
		}
		methods = append(methods, call.Method)
		if pid == 0 {
			pid = call.PID
		}
		require.Equal(t, pid, call.PID, "builder and runtime must share the selected session")
		switch call.Method {
		case "Builder.Configure":
			request := &builderv0.ConfigureRequest{}
			require.NoError(t, protojson.Unmarshal(call.Request, request))
			require.Len(t, request.GetChanges(), 1)
			expected := changes[0]
			if configuredCalls > 0 {
				expected = reset
			}
			require.True(t, proto.Equal(expected, request.GetChanges()[0]))
			configuredCalls++
		case "Runtime.Test":
			request := &runtimev0.TestRequest{}
			require.NoError(t, protojson.Unmarshal(call.Request, request))
			require.Equal(t, "opaque-test-selector", request.GetTarget())
		}
	}
	require.Equal(t, []string{"Builder.Load", "Builder.Configure", "Builder.Configure", "Runtime.Load", "Runtime.Init", "Runtime.Test"}, methods)
}
