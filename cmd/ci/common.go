package ci

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
)

// Silent services in the CLI
var silent []string

// Scope for the runtime: affect ports to avoid conflict with run
// Useful for testing/CI
var scope string

// Runtime context
var runtimeContext string

// load only mode
var loadOnly bool

// init only mode
var initOnly bool

// Test suites selected for CI. Empty means each agent's advertised default.
var testSuites []string

// temporaryPorts asks the runtime allocator for OS-probed ephemeral ports so a
// CI run's port space cannot collide with another run's on the same host.
var temporaryPorts bool

// portOverrideFlags carries raw --override-port entries (endpoint=port) before
// they are parsed into an EndpointDestination→port map.
var portOverrideFlags []string

// parsePortOverrides turns "app/subject/rest=45001" entries into an
// EndpointDestination→port map for orchestration.Flow.WithPortOverrides.
func parsePortOverrides(entries []string) (map[string]uint16, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	overrides := make(map[string]uint16, len(entries))
	for _, entry := range entries {
		destination, raw, ok := strings.Cut(entry, "=")
		destination = strings.TrimSpace(destination)
		raw = strings.TrimSpace(raw)
		if !ok || destination == "" || raw == "" {
			return nil, fmt.Errorf("invalid --override-port %q: want endpoint=port (e.g. app/subject/rest=45001)", entry)
		}
		port, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("invalid --override-port %q: %q is not a valid port", entry, raw)
		}
		overrides[destination] = uint16(port)
	}
	return overrides, nil
}

// Scheduler controls shared by every affected-service validation command.
// Zero jobs selects a conservative machine-derived default.
var (
	ciJobs     int
	ciFailFast bool
)

type Action func(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error

type flowStopper interface {
	Stop() error
}

type flowAgentOwner interface {
	AgentCacheKeys() []string
}

// runAndStopFlow guarantees that every CI flow is stopped exactly once. Both
// the operation and teardown errors are retained so a cleanup failure cannot
// hide the original build/test/deploy failure (or vice versa).
func runAndStopFlow(flow flowStopper, action func() error) (result error) {
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		if flow == nil {
			return nil
		}
		err := flow.Stop()
		if owner, ok := flow.(flowAgentOwner); ok {
			for _, cacheKey := range owner.AgentCacheKeys() {
				services.ClearAgent(cacheKey)
			}
		}
		return err
	}
	defer func() {
		if err := stop(); err != nil {
			result = errors.Join(result, fmt.Errorf("cannot stop flow: %w", err))
		}
	}()

	if action == nil {
		return nil
	}
	return action()
}

func stopFlowAfterError(flow *orchestration.Flow, err error) error {
	return runAndStopFlow(flow, func() error { return err })
}

func CI(ctx context.Context, workspace *resources.Workspace, action Action) error {
	w := wool.Get(ctx).In("deployCI")
	// Load dependencies
	deps, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return w.Wrapf(err, "Cannot load dependencies")
	}
	entryPoints, err := deps.EntryPoints(ctx)
	if err != nil {
		return w.Wrapf(err, "Cannot get entrypoints")
	}
	alreadyRan := make(map[string]bool)
	for _, entry := range entryPoints {
		// Get a topological order of the services up to entryPoint
		order, err := deps.OrderTo(ctx, entry.Unique)
		if err != nil {
			return w.Wrapf(err, "Cannot get order to <%s>", entry.Unique)
		}
		w.Debug("Order", wool.Field("to", entry.Unique), wool.Field("order", order))
		order = append(order, entry)
		for _, svc := range order {
			if alreadyRan[svc.Unique] {
				continue
			}
			w.Info("Handling services", wool.Field("service", svc.Unique))

			ref, err := resources.ParseServiceWithOptionalModule(svc.Unique)
			if err != nil {
				return w.Wrapf(err, "Cannot parse service <%s>", svc.Unique)
			}

			module, err := workspace.LoadModuleFromName(ctx, ref.Module)
			if err != nil {
				return w.Wrapf(err, "Cannot load module <%s>", ref.Module)
			}
			service, err := module.LoadServiceFromName(ctx, ref.Name)
			if err != nil {
				return w.Wrapf(err, "Cannot load service <%s>", svc.Unique)
			}

			service.WithModule(module.Name)

			err = action(ctx, workspace, module, service)
			if err != nil {
				return w.Wrapf(err, "Cannot run CI %T for service <%s>", action, svc.Unique)
			}
			alreadyRan[svc.Unique] = true
		}
	}
	return nil
}

// ScheduleOptions controls bounded affected-service execution. Test phases use
// LockDependencyClosure so two flows cannot concurrently own one runtime
// dependency agent; static and artifact phases only lock their target.
type ScheduleOptions struct {
	Jobs                  int
	FailFast              bool
	LockDependencyClosure bool
	Phase                 string
	Suite                 string
	RuntimeContext        string
	Reporter              *CIReporter
}

// CIWithPlan retains the serial, fail-fast behavior expected by older callers.
// User-facing CI commands call CIWithPlanOptions with their scheduler flags.
func CIWithPlan(ctx context.Context, workspace *resources.Workspace, plan *Plan, action Action) error {
	return CIWithPlanOptions(ctx, workspace, plan, action, ScheduleOptions{Jobs: 1, FailFast: true})
}

type ciScheduledTask struct {
	index          int
	planned        PlannedService
	remaining      int
	dependents     []int
	prerequisites  []string
	failedRequired []string
	resources      []string
}

type ciTaskResult struct {
	index int
	err   error
}

type ciTaskFailure struct {
	index   int
	service string
	err     error
}

// CIWithPlanOptions executes each affected service once, allowing independent
// graph nodes to run concurrently while every selected prerequisite completes
// first. Failures and blocked dependents are reported in deterministic plan
// order regardless of goroutine completion order.
func CIWithPlanOptions(ctx context.Context, workspace *resources.Workspace, plan *Plan, action Action, options ScheduleOptions) error {
	w := wool.Get(ctx).In("affectedCI")
	if plan == nil {
		return w.NewError("CI plan is nil")
	}
	if workspace == nil {
		return w.NewError("workspace is nil")
	}
	if action == nil {
		return w.NewError("CI action is nil")
	}
	if len(plan.Services) == 0 {
		if options.Reporter != nil {
			_, err := options.Reporter.registerTasks(ctx, workspace, options, nil)
			return err
		}
		return nil
	}
	jobs, err := normalizeCIJobs(options.Jobs)
	if err != nil {
		return err
	}
	if jobs > len(plan.Services) {
		jobs = len(plan.Services)
	}
	tasks, err := buildScheduledTasks(ctx, workspace, plan, options.LockDependencyClosure)
	if err != nil {
		return err
	}
	var reportTaskIDs []string
	if options.Reporter != nil {
		reportTaskIDs, err = options.Reporter.registerTasks(ctx, workspace, options, tasks)
		if err != nil {
			return err
		}
	}

	ready := make([]int, 0, len(tasks))
	for i := range tasks {
		if tasks[i].remaining == 0 {
			ready = append(ready, i)
		}
	}
	results := make(chan ciTaskResult, len(tasks))
	activeResources := map[string]bool{}
	running := 0
	settled := 0
	stopScheduling := false
	contextErr := error(nil)
	var failures []ciTaskFailure
	var blocked []string

	var settle func(index int, success bool, blocker string)
	settle = func(index int, success bool, blocker string) {
		queue := []struct {
			index   int
			success bool
			blocker string
		}{{index: index, success: success, blocker: blocker}}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			settled++
			for _, dependent := range tasks[current.index].dependents {
				task := &tasks[dependent]
				task.remaining--
				if !current.success {
					task.failedRequired = append(task.failedRequired, current.blocker)
				}
				if task.remaining != 0 {
					continue
				}
				if len(task.failedRequired) > 0 {
					blockers := sortedUnique(task.failedRequired)
					blocked = append(blocked, fmt.Sprintf("%s (failed prerequisite: %s)", task.planned.Service, blockers[0]))
					if options.Reporter != nil {
						options.Reporter.skipTask(reportTaskIDs[dependent], reportReasonFailedPrerequisite, blockers)
					}
					queue = append(queue, struct {
						index   int
						success bool
						blocker string
					}{index: dependent, blocker: blockers[0]})
					continue
				}
				ready = append(ready, dependent)
				sort.Ints(ready)
			}
		}
	}

	for settled < len(tasks) {
		for running < jobs && !stopScheduling {
			position := firstRunnableTask(ready, tasks, activeResources)
			if position < 0 {
				break
			}
			index := ready[position]
			ready = append(ready[:position], ready[position+1:]...)
			for _, resource := range tasks[index].resources {
				activeResources[resource] = true
			}
			running++
			task := tasks[index]
			taskContext := ctx
			if options.Reporter != nil {
				options.Reporter.startTask(reportTaskIDs[index])
				taskContext = withCIReportTask(ctx, options.Reporter, reportTaskIDs[index])
			}
			go func() {
				results <- ciTaskResult{index: task.index, err: executePlannedService(taskContext, workspace, task.planned, action)}
			}()
		}

		if running == 0 {
			if stopScheduling {
				break
			}
			if len(ready) == 0 {
				return fmt.Errorf("affected-service scheduler stalled: selected dependency graph is cyclic or incomplete")
			}
			return fmt.Errorf("affected-service scheduler stalled on resource ownership")
		}

		select {
		case result := <-results:
			running--
			if options.Reporter != nil {
				options.Reporter.finishTask(reportTaskIDs[result.index], result.err)
			}
			for _, resource := range tasks[result.index].resources {
				delete(activeResources, resource)
			}
			if result.err != nil {
				failures = append(failures, ciTaskFailure{index: result.index, service: tasks[result.index].planned.Service, err: result.err})
				if options.FailFast {
					stopScheduling = true
				}
			}
			settle(result.index, result.err == nil, tasks[result.index].planned.Service)
		case <-ctx.Done():
			if contextErr == nil {
				contextErr = ctx.Err()
			}
			stopScheduling = true
		}
	}

	// A cancellation can win the select repeatedly while workers are cleaning
	// up. Drain every running action before returning so no flow or agent is
	// left behind after the command exits.
	for running > 0 {
		result := <-results
		running--
		if options.Reporter != nil {
			options.Reporter.finishTask(reportTaskIDs[result.index], result.err)
		}
		for _, resource := range tasks[result.index].resources {
			delete(activeResources, resource)
		}
		if result.err != nil {
			failures = append(failures, ciTaskFailure{index: result.index, service: tasks[result.index].planned.Service, err: result.err})
		}
	}

	if len(blocked) > 0 {
		sort.Strings(blocked)
		w.Warn("Skipping services whose selected prerequisites failed", wool.Field("services", blocked))
	}
	if stopScheduling && options.FailFast && settled < len(tasks) {
		w.Info("Fail-fast stopped unscheduled CI tasks", wool.Field("count", len(tasks)-settled))
	}
	if options.Reporter != nil {
		for _, id := range reportTaskIDs {
			if contextErr != nil {
				options.Reporter.cancelPendingTask(id)
				continue
			}
			if stopScheduling && options.FailFast {
				options.Reporter.skipTask(id, reportReasonFailFast, nil)
				continue
			}
		}
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].index < failures[j].index })
	joined := contextErr
	for _, failure := range failures {
		joined = errors.Join(joined, fmt.Errorf("service %s: %w", failure.service, failure.err))
	}
	return joined
}

func normalizeCIJobs(jobs int) (int, error) {
	if jobs < 0 {
		return 0, fmt.Errorf("--jobs must be zero (auto) or a positive integer")
	}
	if jobs > 0 {
		return jobs, nil
	}
	jobs = runtime.GOMAXPROCS(0)
	if jobs > 4 {
		jobs = 4
	}
	if jobs < 1 {
		jobs = 1
	}
	return jobs, nil
}

func buildScheduledTasks(ctx context.Context, workspace *resources.Workspace, plan *Plan, lockDependencyClosure bool) ([]ciScheduledTask, error) {
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return nil, fmt.Errorf("load CI scheduler dependency graph: %w", err)
	}
	tasks := make([]ciScheduledTask, len(plan.Services))
	selected := make(map[string]int, len(plan.Services))
	for index, planned := range plan.Services {
		if _, exists := selected[planned.Service]; exists {
			return nil, fmt.Errorf("CI plan contains duplicate service %q", planned.Service)
		}
		selected[planned.Service] = index
		tasks[index] = ciScheduledTask{index: index, planned: planned, resources: []string{planned.Service}}
		if !lockDependencyClosure {
			continue
		}
		order, err := dependencies.OrderTo(ctx, planned.Service)
		if err != nil {
			return nil, fmt.Errorf("resolve runtime resources for %s: %w", planned.Service, err)
		}
		for _, required := range order {
			tasks[index].resources = append(tasks[index].resources, required.Unique)
		}
		tasks[index].resources = sortedUnique(tasks[index].resources)
	}
	// Preserve transitive ordering even when an intermediate service is not in
	// the affected set (for example two library consumers separated by a
	// non-selected gateway).
	for dependent, dependentTask := range tasks {
		for prerequisite, prerequisiteTask := range tasks {
			if dependent == prerequisite {
				continue
			}
			depends, err := dependencies.DependsOn(dependentTask.planned.Service, prerequisiteTask.planned.Service)
			if err != nil {
				return nil, fmt.Errorf("resolve scheduler ordering between %s and %s: %w", dependentTask.planned.Service, prerequisiteTask.planned.Service, err)
			}
			if !depends {
				continue
			}
			tasks[prerequisite].dependents = append(tasks[prerequisite].dependents, dependent)
			tasks[dependent].prerequisites = append(tasks[dependent].prerequisites, prerequisiteTask.planned.Service)
			tasks[dependent].remaining++
		}
	}
	for i := range tasks {
		sort.Ints(tasks[i].dependents)
		tasks[i].prerequisites = sortedUnique(tasks[i].prerequisites)
	}
	return tasks, nil
}

func firstRunnableTask(ready []int, tasks []ciScheduledTask, activeResources map[string]bool) int {
	for position, index := range ready {
		runnable := true
		for _, resource := range tasks[index].resources {
			if activeResources[resource] {
				runnable = false
				break
			}
		}
		if runnable {
			return position
		}
	}
	return -1
}

func executePlannedService(ctx context.Context, workspace *resources.Workspace, planned PlannedService, action Action) error {
	w := wool.Get(ctx).In("affectedCI")
	ref, err := resources.ParseServiceWithOptionalModule(planned.Service)
	if err != nil {
		return w.Wrapf(err, "Cannot parse planned service <%s>", planned.Service)
	}
	module, err := workspace.LoadModuleFromName(ctx, ref.Module)
	if err != nil {
		return w.Wrapf(err, "Cannot load module <%s>", ref.Module)
	}
	service, err := module.LoadServiceFromName(ctx, ref.Name)
	if err != nil {
		return w.Wrapf(err, "Cannot load service <%s>", planned.Service)
	}
	service.WithModule(module.Name)
	w.Info("Handling affected service",
		wool.Field("service", planned.Service),
		wool.Field("classification", planned.Classification))
	if err := action(ctx, workspace, module, service); err != nil {
		return w.Wrapf(err, "Cannot run CI action for service <%s>", planned.Service)
	}
	return nil
}
