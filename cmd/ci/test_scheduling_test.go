package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func stubTestInspection(t *testing.T, info *agentv0.AgentInformation, err error) {
	t.Helper()
	previous := inspectTestAgent
	t.Cleanup(func() { inspectTestAgent = previous })
	inspectTestAgent = func(_ context.Context, agent *resources.Agent) (*resources.Agent, *agentv0.AgentInformation, error) {
		return agent, info, err
	}
}

func schedulingTestInfo() *agentv0.AgentInformation {
	return &agentv0.AgentInformation{Validation: &agentv0.ValidationCapabilities{
		Test: &agentv0.TestValidationCapability{Supported: true, Suites: []*agentv0.TestSuiteCapability{
			{Name: "unit", DefaultSuite: true, DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE},
			{Name: "integration", DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES},
			{Name: "e2e", DependencyMode: agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK},
		}},
	}}
}

func TestCITestSchedulingUsesSuiteOwnership(t *testing.T) {
	for _, suite := range []string{"", "unit", "integration", "e2e"} {
		t.Run(suite, func(t *testing.T) {
			stubTestInspection(t, schedulingTestInfo(), nil)
			_, workspace := loadSchedulerFixture(t)
			// Reverse dependency order: even a runtime suite needs a dependency's
			// runtime, not a successful result from that dependency's test task.
			plan := &Plan{Services: []PlannedService{{Service: "web/frontend"}, {Service: "management/organization"}}}
			reporter := fixedCIReporter(t, plan)
			options := ScheduleOptions{Jobs: 2, Phase: ciPhaseTest, Suite: suite, LockDependencyClosure: true, Reporter: reporter}
			require.NoError(t, prepareCIReportTasks(t.Context(), workspace, plan, options))
			entered, release := make(chan string, 2), make(chan struct{})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			failed := errors.New("frontend suite failed")
			go func() {
				done <- WithPlanOptions(ctx, workspace, plan, func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, service *resources.Service) error {
					entered <- resources.WithUnique(service).Unique()
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
					if service.Name == "frontend" {
						return failed
					}
					return nil
				}, options)
			}()
			select {
			case first := <-entered:
				if suite == "integration" || suite == "e2e" {
					require.Equal(t, "web/frontend", first)
				}
			case <-ctx.Done():
				t.Fatal("no test started")
			}
			shared := suite == "integration" || suite == "e2e"
			if shared {
				select {
				case second := <-entered:
					t.Fatalf("shared runtime overlapped with %s", second)
				case <-time.After(75 * time.Millisecond):
				}
			} else {
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("dependency-free tests did not overlap")
				}
			}
			close(release)
			require.ErrorIs(t, <-done, failed)
			report := reporter.Finalize(failed)
			assertReportTask(t, report.Tasks[0], reportStatusFailed, "")
			assertReportTask(t, report.Tasks[1], reportStatusPassed, "")
			require.Empty(t, report.Tasks[0].Prerequisites)
			if shared {
				require.Contains(t, report.Tasks[0].RuntimeResources, "management/organization")
			} else {
				require.Equal(t, []string{"web/frontend"}, report.Tasks[0].RuntimeResources)
			}
		})
	}
}

func TestCITestSchedulingRejectsUnresolvedSuites(t *testing.T) {
	for _, failFast := range []bool{false, true} {
		_, workspace := loadSchedulerFixture(t)
		plan := &Plan{Services: []PlannedService{{Service: "web/frontend"}, {Service: "management/organization"}}}
		stubTestInspection(t, schedulingTestInfo(), nil)
		var ran atomic.Bool
		err := WithPlanOptions(t.Context(), workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
			ran.Store(true)
			return nil
		}, ScheduleOptions{Jobs: 1, Phase: ciPhaseTest, Suite: "missing", FailFast: failFast})
		require.ErrorContains(t, err, "not advertised")
		require.False(t, ran.Load())
	}
}

func TestCITestPhaseStillRejectsRuntimeCycle(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	path := filepath.Join(root, "modules/management/services/organization/service.codefly.yaml")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(content, []byte("\nservice-dependencies:\n  - name: frontend\n    module: web\n")...), 0o644))
	stubTestInspection(t, schedulingTestInfo(), nil)
	plan := &Plan{Services: []PlannedService{{Service: "management/organization"}, {Service: "web/frontend"}}}
	var ran atomic.Bool
	err = WithPlanOptions(t.Context(), workspace, plan, func(context.Context, *resources.Workspace, *resources.Module, *resources.Service) error {
		ran.Store(true)
		return nil
	}, ScheduleOptions{Jobs: 2, Phase: ciPhaseTest, Suite: "unit"})
	require.ErrorContains(t, err, "cycle")
	require.False(t, ran.Load())
}

// A fully reusable result must not need its agent: reuse wins over inspection.
func TestCITestReuseDoesNotInspectOrFailOnInspection(t *testing.T) {
	_, workspace := loadReuseFixture(t)
	plan := cacheTestPlan(workspace, "management/consumer")
	store := t.TempDir()
	options := []reuseGateOption{withReusePhase("test"), withReuseSuite("unit")}
	stubTestInspection(t, schedulingTestInfo(), nil)
	cold := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), options...)
	require.Equal(t, 1, cold.executed)

	var inspected atomic.Int32
	inspectTestAgent = func(context.Context, *resources.Agent) (*resources.Agent, *agentv0.AgentInformation, error) {
		inspected.Add(1)
		return nil, nil, errors.New("agent cannot be started")
	}
	warm := runReuseGate(t, workspace, plan, newReuseTestEngine(t, workspace, store, "runner@sha256:aaa", reuseTestReference), options...)
	require.Equal(t, 0, warm.executed)
	require.Equal(t, reportStatusReused, warm.task(t).Status)
	require.Zero(t, inspected.Load())
}

// A replayed plan binds its saved ownership; inspection must not narrow it.
func TestCITestReplayKeepsSavedOwnership(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	stubTestInspection(t, schedulingTestInfo(), nil)
	saved := []string{"management/organization", "web/frontend"}
	plan := &Plan{Services: []PlannedService{{Service: "web/frontend"}}, replay: &ReplayPlan{
		Invocation: ReplayInvocation{Phases: []string{ciPhaseTest}, Suites: []string{"unit"}},
		Tasks:      []ReplayTask{{Phase: ciPhaseTest, Suite: "unit", Service: PlannedService{Service: "web/frontend"}, Resources: saved}},
	}}
	reporter := fixedCIReporter(t, plan)
	options := ScheduleOptions{Jobs: 1, Phase: ciPhaseTest, Suite: "unit", Reporter: reporter}
	require.NoError(t, prepareCIReportTasks(t.Context(), workspace, plan, options))
	var owned []string
	require.NoError(t, WithPlanOptions(t.Context(), workspace, plan, func(ctx context.Context, _ *resources.Workspace, _ *resources.Module, _ *resources.Service) error {
		owned, _ = ctx.Value(testOwnershipKey{}).([]string)
		return nil
	}, options))
	require.Equal(t, saved, owned)
	require.Equal(t, saved, reporter.Finalize(nil).Tasks[0].RuntimeResources)
}
