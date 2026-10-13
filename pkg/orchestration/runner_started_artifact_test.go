package orchestration

import (
	"context"
	"testing"

	agentservices "github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// artifactRuntimeClient answers Start with a chosen status, so a case can be
// about what an agent reports rather than about which agent reports it.
type artifactRuntimeClient struct {
	runtimev0.RuntimeClient
	status *runtimev0.StartStatus
	starts int
}

func (client *artifactRuntimeClient) Start(context.Context, *runtimev0.StartRequest, ...grpc.CallOption) (*runtimev0.StartResponse, error) {
	client.starts++
	return &runtimev0.StartResponse{Status: client.status}, nil
}

func (client *artifactRuntimeClient) Stop(context.Context, *runtimev0.StopRequest, ...grpc.CallOption) (*runtimev0.StopResponse, error) {
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}

// A workload's build is what the agent said it started. The runner keeps that
// answer per service, and the flow serves it by unique: nothing downstream
// reconstructs a path from the agent's on-disk layout, which is what left a
// python service undeliverable (#989).
func TestTheRunnerKeepsWhatTheAgentReportedStarting(t *testing.T) {
	ctx := context.Background()
	reported := &runtimev0.StartedArtifact{
		Executable: "/opt/model/.venv/bin/python",
		Sources:    []string{"/workspace/modules/model/services/model/src/main.py"},
	}
	client := &artifactRuntimeClient{status: &runtimev0.StartStatus{
		State: runtimev0.StartStatus_STARTED, Generation: 1, StartedArtifact: reported,
	}}
	runner, _ := gatewayRunner(t, &agentservices.RuntimeAgent{RuntimeClient: client})

	require.Nil(t, runner.StartedArtifact(), "a runner reports nothing before its service has started")

	_, err := runner.Start(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, client.starts)
	artifact := runner.StartedArtifact()
	require.NotNil(t, artifact)
	require.Equal(t, "/opt/model/.venv/bin/python", artifact.GetExecutable())
	require.Equal(t, reported.GetSources(), artifact.GetSources())

	// The flow answers for the service the manager holds, and only for it.
	flow := &Flow{hub: &Hub{managers: []IManager{&Manager{service: runner.instance.Service, Runner: runner}}}}
	require.Equal(t, artifact, flow.StartedArtifact(runner.Unique()))
	require.Nil(t, flow.StartedArtifact("web/absent"))
	require.Nil(t, (*Flow)(nil).StartedArtifact(runner.Unique()))

	// A restart by an agent that no longer says what it ran must not leave the
	// previous process's answer standing: the build would then be approved for
	// something else entirely. Reporting none is a refusal downstream, which
	// is the honest outcome.
	client.status = &runtimev0.StartStatus{State: runtimev0.StartStatus_STARTED, Generation: 2}
	_, err = runner.Start(ctx)
	require.NoError(t, err)
	require.Nil(t, runner.StartedArtifact())
	require.Nil(t, flow.StartedArtifact(runner.Unique()))
}
