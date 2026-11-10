package ci

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

var inspectTestAgent = services.InspectAgent

type testOwnershipKey struct{}

// Offline plans carry an upper bound on ownership. Refine it before execution
// using the peer's suite contract, without loading or initializing a runtime.
// Failures belong to individual tasks and still obey normal fail-fast/reporting.
//
// A task whose verified result will be reused is never inspected: it keeps the
// conservative closure, so reuse cannot be defeated by an agent that is
// unresolvable or unstartable.
func prepareTestTasks(ctx context.Context, workspace *resources.Workspace, tasks []ciScheduledTask, suite string, reporter *Reporter, ids []string) {
	type inspection struct {
		info *agentv0.AgentInformation
		err  error
	}
	inspected := map[string]inspection{}
	for i := range tasks {
		task := &tasks[i]
		if reporter != nil && reporter.reusable(ids[i]) {
			continue
		}
		_, service, err := loadPlannedService(ctx, workspace, &task.planned)
		if err != nil {
			task.preparationErr = err
			continue
		}
		if service.Agent == nil {
			task.preparationErr = fmt.Errorf("service %s has no agent", task.planned.Service)
			continue
		}
		key := service.Agent.Unique()
		probe, ok := inspected[key]
		if !ok {
			probeCtx, cancel := context.WithTimeout(ctx, manager.DefaultStartupTimeout+2*manager.DefaultDialTimeout)
			_, probe.info, probe.err = inspectTestAgent(probeCtx, service.Agent)
			cancel()
			inspected[key] = probe
		}
		if probe.err != nil {
			task.preparationErr = fmt.Errorf("inspect test agent for %s: %w", task.planned.Service, probe.err)
			continue
		}
		mode, err := orchestration.TestDependencyMode(probe.info, testRequestForSuite(suite, false))
		task.preparationErr = err
		if err == nil && mode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
			task.resources = []string{task.planned.Service}
			if reporter != nil {
				reporter.setTaskResources(ids[i], task.resources)
			}
		}
	}
}
