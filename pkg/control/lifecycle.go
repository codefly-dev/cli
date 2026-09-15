package control

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// This file lifts the Lifecycle group from the cobra commands (cmd/run, cmd/build,
// cmd/test). Every driver follows the same construction the commands use —
// NewFlow(mode) → configure → InitManagers → Load → drive → Stop — so behavior
// matches `codefly run/build/test` exactly. Deploy and Lint/Compile/RunChecks
// are not lifted here yet (Deploy is gated behind MutationAuthority in Phase 2).

// flowTarget is what buildFlow resolved for a lifecycle driver: the workspace
// the flow belongs to, the selected environment, and the module and service it
// targets. Passed as one value because each driver needs a different subset —
// the run driver the module and service, the test driver the environment whose
// declaration carries the fixture — and widening the callback for all of them
// made every driver discard parameters it had no use for.
type flowTarget struct {
	workspace *resources.Workspace
	env       *resources.Environment
	module    *resources.Module
	service   *resources.Service
}

// loadTarget resolves a "module/service" (or bare service) name to its
// workspace, module, and service. It mirrors cmd/common.LoadRequiredE but talks
// straight to core, so pkg/control keeps depending only downward (never on cmd).
func (p *planeImpl) loadTarget(ctx context.Context, name string) (*resources.Workspace, *resources.Module, *resources.Service, error) {
	if name == "" {
		return nil, nil, nil, fmt.Errorf("service name is required")
	}
	ws, err := p.workspace(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	service, module, err := ws.FindUniqueModuleServiceByName(ctx, name)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve service %q: %w", name, err)
	}
	service.WithModule(module.Name)
	return ws, module, service, nil
}

// resolvePinnedModules materializes the composed pinned modules of the plane's
// workspace. The plane loads from an explicit root rather than the process
// working directory, so it cannot share cmd/common's cwd-based helper — but it
// runs the same materialization, so a lifecycle driven here and the same one
// driven from the command line resolve a composed module identically.
func (p *planeImpl) resolvePinnedModules(ctx context.Context) error {
	ws, err := p.workspace(ctx)
	if err != nil {
		return err
	}
	return composition.EnsurePinnedModules(ctx, ws)
}

// buildFlow performs the construction shared by every lifecycle driver: resolve
// the target, select the environment, create the flow in the given mode, apply
// caller configuration, spawn agents (InitManagers), and Load (which builds the
// mode's policy + playbook). The returned flow is ready to drive.
func (p *planeImpl) buildFlow(ctx context.Context, mode orchestration.Mode, name, envName string, configure func(flowTarget, *orchestration.Flow) error) (*orchestration.Flow, error) {
	// Every lifecycle entry point builds its flow here, so this is where the
	// plane's no-narration contract is installed for all of them: without a
	// provider on the context, everything the flow logs falls back to a Console
	// printing to stdout.
	ctx = p.narrationContext(ctx)
	// Resolve composed pinned modules before loadTarget: it finds the service by
	// loading the workspace's modules, and a module composed by identity is not
	// loadable as a checkout until the CLI has pulled it. Without this, driving a
	// run or test through the control plane (the MCP `run_service` and
	// `test_service` tools) fails on exactly the workspaces the command-line
	// entry points now handle.
	if err := p.resolvePinnedModules(ctx); err != nil {
		return nil, err
	}
	ws, module, service, err := p.loadTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	if envName == "" {
		envName = orchestration.LocalEnvironmentName
	}
	env, err := orchestration.SelectEnvironment(ws, envName)
	if err != nil {
		return nil, fmt.Errorf("select environment %q: %w", envName, err)
	}
	flow, err := orchestration.NewFlow(ctx, ws, module, service, env, mode)
	if err != nil {
		return nil, fmt.Errorf("create flow: %w", err)
	}
	if configure != nil {
		if err := configure(flowTarget{workspace: ws, env: env, module: module, service: service}, flow); err != nil {
			return nil, fmt.Errorf("configure flow: %w", err)
		}
	}
	if err := flow.InitManagers(ctx); err != nil {
		return nil, fmt.Errorf("init managers: %w", err)
	}
	if err := flow.Load(ctx); err != nil {
		return nil, fmt.Errorf("load flow: %w", err)
	}
	return flow, nil
}

// stopFlow tears a flow down and clears ONLY that flow's agents. It must not
// call the global services.ClearAgents(): a synchronous driver (Build/Test/
// Lint/Compile/Deploy) can run while a background Run stack is up, and clearing
// every agent would SIGTERM the running stack's plugins out from under it.
func stopFlow(flow *orchestration.Flow) {
	if flow == nil {
		return
	}
	_ = flow.Stop()
	for _, key := range flow.AgentCacheKeys() {
		services.ClearAgent(key)
	}
}

// Build compiles a service's container image (BuildMode). It runs to completion
// and tears down.
func (p *planeImpl) Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	if req.Module != "" && req.Service == "" {
		return BuildResult{}, fmt.Errorf("module-wide build is not yet supported via the control plane; specify a service")
	}
	if req.Push {
		// Push needs registry resolution + login (builder.SetRepository /
		// RegistryLogin), which the CLI's build command wires from --org/env.
		// That plumbing is not lifted yet, so refuse rather than push nowhere.
		return BuildResult{}, fmt.Errorf("push is not yet supported via the control plane")
	}
	// buildFlow installs the contract on its own parameter, which leaves the
	// Build below on the caller's context.
	ctx = p.narrationContext(ctx)
	flow, err := p.buildFlow(ctx, orchestration.BuildMode, req.Service, req.Env, nil)
	if err != nil {
		return BuildResult{}, err
	}
	defer stopFlow(flow)
	if err := flow.Build(ctx); err != nil {
		return BuildResult{Succeeded: false}, fmt.Errorf("build %s: %w", req.Service, err)
	}
	return BuildResult{Succeeded: true}, nil
}

// Test runs a service's tests (TestMode). It drives the flow with Start — the
// TestMode playbook stops after the origin's Test RPC — then reads the origin
// test response, exactly like `codefly test service`.
func (p *planeImpl) Test(ctx context.Context, req TestRequest) (CheckResult, error) {
	testRequest := &runtimev0.TestRequest{Suite: req.Suite}
	if req.Filter != "" {
		testRequest.Filters = []string{req.Filter}
	}
	// buildFlow installs the contract on its own parameter, which leaves the
	// Start below on the caller's context — the drive phase, which is where a
	// test run does most of its narrating.
	ctx = p.narrationContext(ctx)
	flow, err := p.buildFlow(ctx, orchestration.TestMode, req.Service, req.Env, func(target flowTarget, f *orchestration.Flow) error {
		if req.RuntimeContext != "" {
			f.WithRuntimeContext(req.RuntimeContext)
		}
		// The same resolution `codefly test service` performs: an explicit
		// override wins, otherwise the selected environment's declared fixture.
		// Without this a workspace resolved one fixture from the command line
		// and none at all through the control plane / MCP `test` tool.
		f.WithFixture(orchestration.SelectedFixture(target.env, req.Fixture))
		f.WithTestRequest(testRequest)
		return nil
	})
	if err != nil {
		return CheckResult{}, err
	}
	defer stopFlow(flow)
	if err := flow.Start(ctx); err != nil {
		return CheckResult{}, fmt.Errorf("test %s: %w", req.Service, err)
	}
	if flow.OriginTestSkipped() {
		return CheckResult{
			Passed: true,
			Output: fmt.Sprintf("%s advertises no test suites; skipped", req.Service),
		}, nil
	}
	resp := flow.OriginTestResponse()
	return CheckResult{
		Passed: orchestration.TestSucceeded(resp),
		Output: orchestration.RenderTestReport(resp),
	}, nil
}

// Run starts a service and its dependency graph (RunMode). Flow.Start blocks for
// the stack's lifetime, so it runs in the background and Run returns once the
// stack is up (or, when req.Wait is set, once it is healthy). The plane's
// WorkspaceHost owns the flow so later status/stop calls do not depend on
// process-global state. The caller MUST pass a context that governs the run's
// lifetime — cancelling it stops the stack.
func (p *planeImpl) Run(ctx context.Context, req RunRequest) (RunHandle, error) {
	if p.host == nil || p.host.Flows() == nil {
		return RunHandle{}, fmt.Errorf("control plane has no workspace host")
	}
	// Injected here as well as in buildFlow because the background goroutine
	// below derives its context from this one, not from buildFlow's.
	ctx = p.narrationContext(ctx)
	flows := p.host.Flows()
	flow, err := p.buildFlow(ctx, orchestration.RunMode, req.Service, orchestration.LocalEnvironmentName, func(target flowTarget, f *orchestration.Flow) error {
		profile, err := target.workspace.ResolveRunProfile(ctx, req.Profile, resources.RunProfile{ExcludeDependencies: req.Exclude})
		if err != nil {
			return err
		}
		if err := f.WithRunProfile(profile); err != nil {
			return err
		}
		derived, err := solutionrun.DerivedRunInputs(ctx, target.workspace, target.module, target.service,
			resources.WithUnique(target.service).Unique())
		if err != nil {
			return err
		}
		// derived.Notes stay unrendered here. This process may be serving
		// JSON-RPC on stdout, where narration corrupts the stream; the plane has
		// no terminal to write to, and the run command renders them instead.
		f.WithOverrides(derived.Overrides)
		f.WithWorkspaceConfigurationValues(derived.WorkspaceConfigurations)
		if req.RuntimeContext != "" {
			f.WithRuntimeContext(req.RuntimeContext)
		}
		f.WithOutputEnv(req.OutputEnv)
		if req.OutputEnvService != "" {
			if req.OutputEnv == "" {
				return fmt.Errorf("output environment service requires an output environment path")
			}
			if _, err := f.ServiceFromUnique(req.OutputEnvService); err != nil {
				return fmt.Errorf("select output environment service %q: %w", req.OutputEnvService, err)
			}
			f.WithOutputEnvService(req.OutputEnvService)
		}
		f.WithExcludeRoot(req.ExcludeRoot)
		return nil
	})
	if err != nil {
		return RunHandle{}, err
	}
	flowID := req.Service
	if err := flows.Register(flowID, flow); err != nil {
		stopFlow(flow)
		return RunHandle{}, err
	}
	// A fresh run supersedes whatever an earlier, unrelated run left behind;
	// otherwise a stale failure could be reported for a run that hasn't
	// reported anything of its own yet.
	p.clearRunOutcome()
	// Buffered so the final Start result never blocks the goroutine, even when
	// nobody is waiting (req.Wait == false).
	started := make(chan error, 1)
	// The run gets its own cancel so Stop and Close can end this goroutine
	// without depending on the caller cancelling ctx: nothing else releases it,
	// because Playbook.Work returns only once its context is done. Derived from
	// ctx, so a caller cancelling still ends the run exactly as before.
	runCtx, runCancel := context.WithCancel(ctx)
	run := p.trackRun(flowID, runCancel)
	go func() {
		defer p.finishRun(flowID, run)
		defer runCancel()
		err := flow.Start(runCtx)
		started <- err
		if flows.Release(flowID, flow) {
			stopFlow(flow)
			// The flow is no longer in the registry, so FlowStatus can no
			// longer see it at all (Active() would just report FlowIdle,
			// indistinguishable from "nothing was ever run"). Record how it
			// ended so a caller polling FlowStatus — e.g. run_service's
			// wait loop — observes FlowFailed/FlowStopped instead of hanging
			// until it times out waiting for a state that will never come.
			state := FlowStopped
			if err != nil {
				state = FlowFailed
			}
			p.recordRunOutcome(flowID, state, err)
		}
	}()

	if req.Wait {
		if err := waitReady(ctx, flow, started); err != nil {
			_, _ = flows.Stop(flowID, false)
			// Tearing the flow down does not release the goroutine above, which
			// would go on narrating after Run has already failed.
			p.stopRun(flowID)
			return RunHandle{}, err
		}
	}
	return RunHandle{FlowID: flowID}, nil
}

// waitReady blocks until the flow reports ready, the flow exits/fails, or ctx is
// done. Giving up names the requirement that never held — which service, which
// endpoint, which predicate — so a timeout is diagnosable.
func waitReady(ctx context.Context, flow *orchestration.Flow, started <-chan error) error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	var pending *orchestration.ReadinessFailure
	for {
		select {
		case <-ctx.Done():
			if pending != nil {
				return fmt.Errorf("flow not ready (%s): %w", pending, ctx.Err())
			}
			return ctx.Err()
		case err := <-started:
			if err != nil {
				return fmt.Errorf("flow exited before becoming ready: %w", err)
			}
			return fmt.Errorf("flow stopped before becoming ready")
		case <-ticker.C:
			failure := flow.Readiness(ctx)
			if failure == nil {
				return nil
			}
			// A probe interrupted by this very ctx reports the interruption,
			// not the requirement: keep the last diagnosis made while the
			// deadline still had room.
			if ctx.Err() == nil {
				pending = failure
			}
		}
	}
}

// Stop stops one host-owned flow: the one named by req.FlowID, or the
// most-recently-started flow when FlowID is empty. It reports whether
// anything was actually stopped. NameFilter remains a service-level filter
// and is not yet supported by orchestration.
func (p *planeImpl) Stop(_ context.Context, req StopRequest) (bool, error) {
	if p.host == nil || p.host.Flows() == nil {
		return false, nil
	}
	id := req.FlowID
	if id == "" {
		id, _ = p.host.Flows().Active()
		if id == "" {
			// No flow is registered, but that is not the same as nothing
			// running: a flow that exited on its own released the registry
			// entry from inside Run's goroutine, which then goes on tearing
			// down and narrating. Active() is empty exactly then, so returning
			// here without joining is how a caller gets told "nothing running"
			// while the run is still writing to stdout. Nothing else is left to
			// name, so join every run this plane started.
			p.stopAllRuns()
			return false, nil // nothing registered; anything dying is now joined
		}
	}
	stopped, err := p.host.Flows().Stop(id, req.Destroy)
	// The flow is torn down, but Run's goroutine is still unwinding and still
	// narrating. Joining it here is what makes a returned Stop mean that
	// nothing from this run is left running.
	p.stopRun(id)
	if err != nil {
		return stopped, fmt.Errorf("stop flow: %w", err)
	}
	return stopped, nil
}
