package ci

import (
	"context"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// Admit the entire selected request before dispatching any phase or job. Use
// normal flow selection for configuration-reference participants and environment
// exclusions; this constructs no agent managers. Each actual flow checks again.
func checkDisposablePlan(ctx context.Context, workspace *resources.Workspace, plan *Plan) error {
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
