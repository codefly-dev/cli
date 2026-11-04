package orchestration_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/pkg/orchestration"
)

// recordingExecutor stands in for the builders of a snapshot. It records what
// it was asked to do and when, so a test can see how many builds were in
// flight at once and in what order the playbook settled them.
type recordingExecutor struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	finished []string

	// delay is how long one action takes, which is how a test chooses the
	// order the concurrent builds complete in.
	delay func(orchestration.Action) time.Duration
	// failures are the services whose build fails, by unique.
	failures map[string]bool
}

func (r *recordingExecutor) GetExecutor(_ context.Context, action orchestration.Action) (orchestration.OutputProcessorFunc, error) {
	return func(_ context.Context) (*orchestration.OutputProperty, error) {
		building := action.Type == orchestration.BuilderBuild
		if building {
			r.enter()
			defer r.leave()
		}
		if r.delay != nil {
			time.Sleep(r.delay(action))
		}
		r.mu.Lock()
		r.finished = append(r.finished, fmt.Sprintf("%s:%s", action.Type, action.Service))
		r.mu.Unlock()
		if building && r.failures[action.Service] {
			return nil, fmt.Errorf("the image of %s cannot be built", action.Service)
		}
		return orchestration.OnInit(), nil
	}, nil
}

func (r *recordingExecutor) enter() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight++
	if r.inFlight > r.peak {
		r.peak = r.inFlight
	}
}

func (r *recordingExecutor) leave() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight--
}

func (r *recordingExecutor) peakInFlight() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak
}

func (r *recordingExecutor) completionOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.finished...)
}

// snapshotClosure is the number of services a snapshot rooted at unique
// carries, which is how many actions each of its phases has.
func snapshotClosure(t *testing.T, dependencies *architecture.ServiceDependencies, unique string) int {
	t.Helper()
	restricted, err := dependencies.Restrict(context.Background(), unique)
	require.NoError(t, err)
	stage, err := restricted.ForStage(resources.StageRun)
	require.NoError(t, err)
	order, err := stage.Graph().TopologicalSort()
	require.NoError(t, err)
	return len(order)
}

// runSnapshot drives a whole snapshot through the playbook and returns what it
// executed, with the executor that watched it.
func runSnapshot(t *testing.T, root string, delay func(orchestration.Action) time.Duration, failures map[string]bool) ([]orchestration.Action, *recordingExecutor, error) {
	t.Helper()
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())
	services := snapshotClosure(t, data.world.Dependencies, root)
	require.Greater(t, services, 1, "this test needs a root with dependencies to have anything to parallelize")

	executor := &recordingExecutor{delay: delay, failures: failures}
	policy, err := orchestration.NewSnapshotPolicy(ctx, data.world.Dependencies, executor)
	require.NoError(t, err)

	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)
	playbook.WithPolicy(policy)
	// The plan ends with one deploy per service; stopping on the last of them
	// is what flow.go's snapshot stopping policy does, without reaching into
	// the policy's unexported plan.
	deployed := 0
	playbook.WithStoppingAfter(func(_ context.Context, action orchestration.Action) bool {
		if action.Type == orchestration.BuilderDeploy {
			deployed++
		}
		return deployed == services
	})

	runErr := playbook.Begin(ctx, orchestration.Action{Type: orchestration.BuilderBegin, Service: root})
	return playbook.Executed(), executor, runErr
}

// The root of this workspace's deepest chain: web/frontend -> web/gateway ->
// billing/accounts -> management/organization.
const concurrentRoot = "web/frontend"

func TestSnapshotBuildsRunAtTheSameTime(t *testing.T) {
	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "4")

	// Every build sleeps, so a build phase that ran one at a time could not
	// show more than one in flight.
	hold := func(action orchestration.Action) time.Duration {
		if action.Type == orchestration.BuilderBuild {
			return 150 * time.Millisecond
		}
		return 0
	}
	_, executor, err := runSnapshot(t, concurrentRoot, hold, nil)
	require.NoError(t, err)
	require.Greater(t, executor.peakInFlight(), 1, "the build phase did not run any two builds together")
}

func TestSnapshotBuildsHonourTheBound(t *testing.T) {
	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "2")
	hold := func(action orchestration.Action) time.Duration {
		if action.Type == orchestration.BuilderBuild {
			return 100 * time.Millisecond
		}
		return 0
	}
	_, executor, err := runSnapshot(t, concurrentRoot, hold, nil)
	require.NoError(t, err)
	require.Equal(t, 2, executor.peakInFlight(), "the bound was not applied")
}

func TestSnapshotBuildsOneAtATimeWhenPinnedToOne(t *testing.T) {
	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "1")
	hold := func(action orchestration.Action) time.Duration {
		if action.Type == orchestration.BuilderBuild {
			return 20 * time.Millisecond
		}
		return 0
	}
	_, executor, err := runSnapshot(t, concurrentRoot, hold, nil)
	require.NoError(t, err)
	require.Equal(t, 1, executor.peakInFlight())
}

// A render feeds a signed publish, so what the flow records must be a function
// of the plan and not of which build finished first.
func TestConcurrentBuildsDoNotChangeWhatTheFlowRecords(t *testing.T) {
	serial := func(_ orchestration.Action) time.Duration { return 0 }
	// Later services finish first, which is the order a one-at-a-time phase
	// could never produce.
	reversed := func() func(orchestration.Action) time.Duration {
		position := map[string]int{}
		var mu sync.Mutex
		return func(action orchestration.Action) time.Duration {
			if action.Type != orchestration.BuilderBuild {
				return 0
			}
			mu.Lock()
			defer mu.Unlock()
			if _, seen := position[action.Service]; !seen {
				position[action.Service] = len(position)
			}
			// The first build started sleeps the longest.
			return time.Duration(40-10*position[action.Service]) * time.Millisecond
		}
	}

	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "1")
	oneAtATime, _, err := runSnapshot(t, concurrentRoot, serial, nil)
	require.NoError(t, err)

	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "8")
	together, executor, err := runSnapshot(t, concurrentRoot, reversed(), nil)
	require.NoError(t, err)
	require.Greater(t, executor.peakInFlight(), 1, "nothing ran together, so this proves nothing")

	require.Equal(t, oneAtATime, together,
		"the actions a snapshot records depend on how many builds ran at once")

	// And repeated concurrent runs agree with each other, whatever the host's
	// scheduler did on any one of them.
	for attempt := range 3 {
		again, _, againErr := runSnapshot(t, concurrentRoot, reversed(), nil)
		require.NoError(t, againErr)
		require.Equal(t, together, again, "concurrent run %d disagreed with the first", attempt)
	}
}

func TestConcurrentBuildFailureNamesEveryServiceThatFailed(t *testing.T) {
	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "4")
	failures := map[string]bool{"web/gateway": true, "billing/accounts": true}
	_, executor, err := runSnapshot(t, concurrentRoot, nil, failures)
	require.Error(t, err)

	// Both failures, not just whichever lost the race.
	require.Contains(t, err.Error(), "web/gateway")
	require.Contains(t, err.Error(), "billing/accounts")
	require.Contains(t, err.Error(), "2 of")
	require.Contains(t, err.Error(), "the image of web/gateway cannot be built")
	require.Contains(t, err.Error(), "the image of billing/accounts cannot be built")

	// A failing build does not cancel its siblings: every service's build ran.
	builds := 0
	for _, done := range executor.completionOrder() {
		if strings.HasPrefix(done, string(orchestration.BuilderBuild)+":") {
			builds++
		}
	}
	require.Equal(t, 4, builds, "a failing build cut its siblings short")
}

func TestConcurrentBuildFailureBlamesABuildAndNotTheOrigin(t *testing.T) {
	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "4")
	service, phase, ok := failedServiceOf(t, concurrentRoot, map[string]bool{"billing/accounts": true})
	require.True(t, ok)
	require.Equal(t, "billing/accounts", service)
	require.Equal(t, orchestration.BuilderBuild, phase)
}

// failedServiceOf re-runs a failing snapshot and reports what the playbook
// attributed the failure to.
func failedServiceOf(t *testing.T, root string, failures map[string]bool) (string, orchestration.ActionType, bool) {
	t.Helper()
	ctx := context.Background()
	data := setup(t, orchestration.BuilderSync, execOnInit())
	executor := &recordingExecutor{failures: failures}
	policy, err := orchestration.NewSnapshotPolicy(ctx, data.world.Dependencies, executor)
	require.NoError(t, err)
	playbook, err := orchestration.NewPlaybook(ctx, data.world)
	require.NoError(t, err)
	playbook.WithPolicy(policy)
	require.Error(t, playbook.Begin(ctx, orchestration.Action{Type: orchestration.BuilderBegin, Service: root}))
	return playbook.FailedService()
}

// A mistyped override must stop the render, not quietly give back the
// one-at-a-time behaviour the operator was trying to leave behind.
func TestAMistypedBoundRefusesTheRender(t *testing.T) {
	t.Setenv("CODEFLY_BUILD_CONCURRENCY", "four")
	_, _, err := runSnapshot(t, concurrentRoot, nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CODEFLY_BUILD_CONCURRENCY")
}
