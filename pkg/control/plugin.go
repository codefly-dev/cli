package control

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/agents/manager"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
)

// This file lifts the plugin-backed Code operations: Fix (language-aware repair)
// and DependencyManager (language package deps — Go modules, npm, …). These ARE
// special, language-specific behavior, so they go through the service's Code
// plugin over gRPC. Unlike CommandRunner (which reaches the agent via
// services.Load), the Code client is not exposed on services.Instance, so this
// uses the manager.Load acquisition the Gateway also uses.

// withCodeClient loads the service's plugin agent, hands its Code client to fn,
// and tears the agent down afterward. It loads and closes per call (no caching
// yet); the Gateway caches per service for throughput, which the plane can adopt
// later.
func (p *planeImpl) withCodeClient(ctx context.Context, name string, fn func(codev0.CodeClient) error) error {
	ws, _, service, err := p.loadTarget(ctx, name)
	if err != nil {
		return err
	}
	agent := service.Agent
	if agent == nil {
		return fmt.Errorf("service %q has no agent", name)
	}
	if _, err := manager.ResolveLatest(ctx, agent); err != nil {
		return fmt.Errorf("resolve agent %s: %w", agent.Name, err)
	}
	conn, err := manager.Load(ctx, agent,
		manager.WithWorkDir(ws.Dir()),
		// A locally-run plugin needs the developer process's ambient
		// filesystem/runtime access and is not a Toolbox principal — same as the
		// Gateway's local load.
		manager.WithoutSandbox(),
		manager.WithoutPrincipal(),
	)
	if err != nil {
		return fmt.Errorf("load agent %s: %w", agent.Name, err)
	}
	defer conn.Close()
	return fn(codev0.NewCodeClient(conn.GRPCConn()))
}

// Fix runs the language plugin's safe (or aggressive) auto-repairs over a file.
func (p *planeImpl) Fix(ctx context.Context, req FixRequest) (FixResult, error) {
	mode := basev0.FixMode_FIX_MODE_SAFE
	if req.Aggressive {
		mode = basev0.FixMode_FIX_MODE_AGGRESSIVE
	}
	var result FixResult
	err := p.withCodeClient(ctx, req.Service, func(code codev0.CodeClient) error {
		resp, err := code.Execute(ctx, &codev0.CodeRequest{
			Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{
				File:   req.File,
				Mode:   mode,
				DryRun: req.DryRun,
			}},
		})
		if err != nil {
			return fmt.Errorf("fix %s: %w", req.File, err)
		}
		r := resp.GetFix()
		if r == nil {
			return fmt.Errorf("fix returned no result")
		}
		if r.GetChanged() {
			result.Changed = []string{req.File}
		}
		result.Output = r.GetOutput()
		return nil
	})
	return result, err
}

// ListDependencies returns the service's language package dependencies.
func (p *planeImpl) ListDependencies(ctx context.Context, service string) ([]Dependency, error) {
	var deps []Dependency
	err := p.withCodeClient(ctx, service, func(code codev0.CodeClient) error {
		resp, err := code.Execute(ctx, &codev0.CodeRequest{
			Operation: &codev0.CodeRequest_ListDependencies{ListDependencies: &codev0.ListDependenciesRequest{}},
		})
		if err != nil {
			return fmt.Errorf("list dependencies: %w", err)
		}
		for _, d := range resp.GetListDependencies().GetDependencies() {
			deps = append(deps, Dependency{Name: d.GetName(), Version: d.GetVersion(), Direct: d.GetDirect()})
		}
		return nil
	})
	return deps, err
}

// AddDependency adds a language package dependency to the service.
func (p *planeImpl) AddDependency(ctx context.Context, service string, dep Dependency) error {
	return p.withCodeClient(ctx, service, func(code codev0.CodeClient) error {
		resp, err := code.Execute(ctx, &codev0.CodeRequest{
			Operation: &codev0.CodeRequest_AddDependency{AddDependency: &codev0.AddDependencyRequest{
				PackageName: dep.Name,
				Version:     dep.Version,
			}},
		})
		if err != nil {
			return fmt.Errorf("add dependency %s: %w", dep.Name, err)
		}
		if !resp.GetAddDependency().GetSuccess() {
			return fmt.Errorf("add dependency %s failed", dep.Name)
		}
		return nil
	})
}

// RemoveDependency removes a language package dependency from the service.
func (p *planeImpl) RemoveDependency(ctx context.Context, service string, dep Dependency) error {
	return p.withCodeClient(ctx, service, func(code codev0.CodeClient) error {
		resp, err := code.Execute(ctx, &codev0.CodeRequest{
			Operation: &codev0.CodeRequest_RemoveDependency{RemoveDependency: &codev0.RemoveDependencyRequest{
				PackageName: dep.Name,
			}},
		})
		if err != nil {
			return fmt.Errorf("remove dependency %s: %w", dep.Name, err)
		}
		if !resp.GetRemoveDependency().GetSuccess() {
			return fmt.Errorf("remove dependency %s failed", dep.Name)
		}
		return nil
	})
}
