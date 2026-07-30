package control

import (
	"context"
	"fmt"
	"time"

	"github.com/codefly-dev/cli/pkg/orchestration"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// This file lifts the Lifecycle group from the cobra commands (cmd/run, cmd/build,
// cmd/test). Every driver follows the same construction the commands use —
// NewFlow(mode) → configure → InitManagers → Load → drive → Stop — so behavior
// matches `codefly run/build/test` exactly. Deploy and Lint/Compile/RunChecks
// are not lifted here yet (Deploy is gated behind MutationAuthority in Phase 2).

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

// buildFlow performs the construction shared by every lifecycle driver: resolve
// the target, select the environment, create the flow in the given mode, apply
// caller configuration, spawn agents (InitManagers), and Load (which builds the
// mode's policy + playbook). The returned flow is ready to drive.
func (p *planeImpl) buildFlow(ctx context.Context, mode orchestration.Mode, name, envName string, configure func(*resources.Workspace, *orchestration.Flow) error) (*orchestration.Flow, error) {
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
		if err := configure(ws, flow); err != nil {
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
	flow, err := p.buildFlow(ctx, orchestration.TestMode, req.Service, orchestration.LocalEnvironmentName, func(_ *resources.Workspace, f *orchestration.Flow) error {
		if req.RuntimeContext != "" {
			f.WithRuntimeContext(req.RuntimeContext)
		}
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
	flows := p.host.Flows()
	flow, err := p.buildFlow(ctx, orchestration.RunMode, req.Service, orchestration.LocalEnvironmentName, func(workspace *resources.Workspace, f *orchestration.Flow) error {
		profile, err := orchestration.ResolveRunProfile(ctx, workspace, req.Profile, req.Exclude)
		if err != nil {
			return err
		}
		if err := f.WithRunProfile(profile); err != nil {
			return err
		}
		if req.RuntimeContext != "" {
			f.WithRuntimeContext(req.RuntimeContext)
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
	// Buffered so the final Start result never blocks the goroutine, even when
	// nobody is waiting (req.Wait == false).
	started := make(chan error, 1)
	go func() {
		err := flow.Start(ctx)
		started <- err
		if flows.Release(flowID, flow) {
			stopFlow(flow)
		}
	}()

	if req.Wait {
		if err := waitReady(ctx, flow, started); err != nil {
			_ = flows.Stop(flowID, false)
			return RunHandle{}, err
		}
	}
	return RunHandle{FlowID: flowID}, nil
}

// waitReady blocks until the flow reports ready, the flow exits/fails, or ctx is
// done — mirroring the readiness poll + failure select the run command uses.
func waitReady(ctx context.Context, flow *orchestration.Flow, started <-chan error) error {
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-started:
			if err != nil {
				return fmt.Errorf("flow exited before becoming ready: %w", err)
			}
			return fmt.Errorf("flow stopped before becoming ready")
		case f := <-flow.Failures():
			return fmt.Errorf("service failure in %s: %s", f.Service, f.Message)
		case <-ticker.C:
			if flow.Ready(ctx) {
				return nil
			}
		}
	}
}

// Stop stops the host-owned active flow. NameFilter remains a service-level
// filter and is not yet supported by orchestration.
func (p *planeImpl) Stop(ctx context.Context, req StopRequest) error {
	if p.host == nil || p.host.Flows() == nil {
		return nil
	}
	id, flow := p.host.Flows().Active()
	if flow == nil {
		return nil // nothing running
	}
	if err := p.host.Flows().Stop(id, req.Destroy); err != nil {
		return fmt.Errorf("stop flow: %w", err)
	}
	return nil
}
