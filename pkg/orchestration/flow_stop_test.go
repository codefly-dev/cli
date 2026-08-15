package orchestration

import (
	"context"
	"testing"
	"time"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// recordingManager is a stand-in IManager that counts how often the flow asks
// it to Stop/Destroy. It lets us assert that shutdown reaches every runner the
// hub knows about — the invariant that keeps a failed/interrupted run from
// leaking the process groups its already-spawned agents hold.
type recordingManager struct {
	unique       string
	stopCalls    int
	destroyCalls int
}

func (m *recordingManager) Unique() string { return m.unique }

func (m *recordingManager) RunnerDoStop(context.Context) (*OutputProperty, error) {
	m.stopCalls++
	return &OutputProperty{}, nil
}

func (m *recordingManager) RunnerDoDestroy(context.Context) (*OutputProperty, error) {
	m.destroyCalls++
	return &OutputProperty{}, nil
}

func (m *recordingManager) BuilderDoInit(context.Context) (*OutputProperty, error)       { return nil, nil }
func (m *recordingManager) BuilderDoLoad(context.Context) (*OutputProperty, error)       { return nil, nil }
func (m *recordingManager) BuilderDoBuild(context.Context) (*OutputProperty, error)      { return nil, nil }
func (m *recordingManager) BuilderDoSync(context.Context) (*OutputProperty, error)       { return nil, nil }
func (m *recordingManager) BuilderDoDeploy(context.Context) (*OutputProperty, error)     { return nil, nil }
func (m *recordingManager) RunnerDoLoad(context.Context) (*OutputProperty, error)        { return nil, nil }
func (m *recordingManager) RunnerDoInit(context.Context) (*OutputProperty, error)        { return nil, nil }
func (m *recordingManager) RunnerDoStart(context.Context) (*OutputProperty, error)       { return nil, nil }
func (m *recordingManager) RunnerDoBuild(context.Context) (*OutputProperty, error)       { return nil, nil }
func (m *recordingManager) RunnerDoLint(context.Context) (*OutputProperty, error)        { return nil, nil }
func (m *recordingManager) RunnerDoTest(context.Context) (*OutputProperty, error)        { return nil, nil }
func (m *recordingManager) RunnerTestResponse() *runtimev0.TestResponse                  { return nil }
func (m *recordingManager) DoSetCallback(func(ctx context.Context, action Action) error) {}
func (m *recordingManager) DoSetFailureSink(func(unique, msg string))                    {}

var _ IManager = &recordingManager{}

// TestFlowStopTearsDownEveryRunnerInHub is the regression guard for the leak:
// InitManagers now registers each manager in flow.hub the moment its agent is
// spawned, so a run that fails partway still hands Stop the runners it already
// created. Whatever subset the hub holds, Stop must reach all of them.
func TestFlowStopTearsDownEveryRunnerInHub(t *testing.T) {
	m1 := &recordingManager{unique: "web/a"}
	m2 := &recordingManager{unique: "web/b"}
	m3 := &recordingManager{unique: "web/c"}
	flow := &Flow{hub: &Hub{managers: []IManager{m1, m2, m3}}}

	require.NoError(t, flow.Stop())

	require.Equal(t, 1, m1.stopCalls)
	require.Equal(t, 1, m2.stopCalls)
	require.Equal(t, 1, m3.stopCalls)
}

func TestFlowStopBuilderModeDoesNotCallMissingRuntime(t *testing.T) {
	manager := &recordingManager{unique: "web/frontend"}
	flow := &Flow{
		world: &World{Mode: BuildMode},
		hub:   &Hub{managers: []IManager{manager}},
	}
	require.NoError(t, flow.Stop())
	require.Zero(t, manager.stopCalls)
	require.Equal(t, []string{"web/frontend"}, flow.AgentCacheKeys())
}

// TestFlowShutdownDestroysEveryRunnerInHub mirrors the above for Destroy.
func TestFlowShutdownDestroysEveryRunnerInHub(t *testing.T) {
	m1 := &recordingManager{unique: "web/a"}
	m2 := &recordingManager{unique: "web/b"}
	flow := &Flow{hub: &Hub{managers: []IManager{m1, m2}}}

	require.NoError(t, flow.Shutdown())

	require.Equal(t, 1, m1.destroyCalls)
	require.Equal(t, 1, m2.destroyCalls)
}

// TestFlowStopNilHubIsNoop covers the pre-InitManagers failure path: when a run
// fails before any manager is created, initRunService still returns a non-nil
// flow whose hub is nil. stopFresh() must treat that as a clean no-op rather
// than panicking (which would abort teardown of anything else).
func TestFlowStopNilHubIsNoop(t *testing.T) {
	require.NoError(t, (&Flow{}).Stop())
	require.NoError(t, (&Flow{}).Shutdown())

	var nilFlow *Flow
	require.NoError(t, nilFlow.Stop())
	require.NoError(t, nilFlow.Shutdown())
}

type blockingFailurePolicy struct {
	started chan struct{}
}

func (p *blockingFailurePolicy) GetExecutor(context.Context, Action) (OutputProcessorFunc, error) {
	return nil, nil
}

func (p *blockingFailurePolicy) Restrict(context.Context, string) error {
	return nil
}

func (p *blockingFailurePolicy) Execute(ctx context.Context, _ Action) ([]Action, error) {
	close(p.started)
	<-ctx.Done()
	return nil, nil
}

func TestFlowStartReturnsPostStartRunnerFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	service := &resources.Service{Name: "api"}
	service.WithModule("backend")
	policy := &blockingFailurePolicy{started: make(chan struct{})}
	playbook, err := NewPlaybook(ctx, &World{})
	require.NoError(t, err)
	playbook.WithPolicy(policy)
	flow := &Flow{
		originService: service,
		playbook:      playbook,
		failures:      make(chan FlowFailure, 1),
	}

	result := make(chan error, 1)
	go func() { result <- flow.Start(ctx) }()
	<-policy.started
	flow.reportFailure("backend/auth-sidecar", "runner exited")

	require.ErrorContains(t, <-result, "service failed after start: backend/auth-sidecar: runner exited")
}
