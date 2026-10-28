package ci

import (
	"context"
	"slices"

	"github.com/codefly-dev/core/architecture"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// Admit the entire selected request before dispatching any phase or job. Use
// normal flow selection for configuration-reference participants and environment
// exclusions; this constructs no agent managers. Each actual flow checks again.
func checkDisposablePlan(ctx context.Context, workspace *resources.Workspace, plan *Plan, phases ...string) error {
	// Only build scheduling expands prerequisites into additional tasks. Its
	// StageBuild closure is actual dispatched work, even if a Flow excludes a
	// bridge. Do not traverse raw runtime branches that no Flow would start.
	if slices.Contains(phases, string(resources.PhaseBuild)) {
		dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
		if err != nil {
			return err
		}
		roots := make([]string, 0, len(plan.Services))
		for _, selected := range plan.Services {
			roots = append(roots, selected.Service)
		}
		if err := orchestration.CheckDisposableImageBuildPrerequisites(ctx, dependencies, roots); err != nil {
			return err
		}
	}

	cache, err := buildCacheFlags.Policy()
	if err != nil {
		return err
	}
	for _, selected := range plan.Services {
		ref, err := resources.ParseServiceWithOptionalModule(selected.Service)
		if err != nil {
			return err
		}
		module, err := workspace.LoadModuleFromName(ctx, ref.Module)
		if err != nil {
			return err
		}
		service, err := module.LoadServiceFromName(ctx, ref.Name)
		if err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
		if err != nil {
			return err
		}
		flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, orchestration.TestMode)
		if err != nil {
			return err
		}
		flow.WithDisposableImages()
		flow.WithBuildCache(cache)
		if err := flow.CheckDisposableImageInputs(ctx); err != nil {
			return err
		}
	}
	return nil
}
