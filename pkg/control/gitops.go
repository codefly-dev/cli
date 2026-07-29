package control

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/internal/mutationauthority"
	"github.com/codefly-dev/cli/pkg/orchestration"
)

func (p *planeImpl) RenderGitOps(ctx context.Context, request GitOpsRenderRequest) (gitops.RenderResult, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.RenderResult{}, err
	}
	envName := request.Env
	if envName == "" {
		envName = orchestration.LocalEnvironmentName
	}
	env, err := orchestration.SelectEnvironment(workspace, envName)
	if err != nil {
		return gitops.RenderResult{}, fmt.Errorf("select environment %q: %w", envName, err)
	}
	if request.Module == "" {
		return gitops.RenderResult{}, fmt.Errorf("module is required")
	}
	module, err := workspace.LoadModuleFromName(ctx, request.Module)
	if err != nil {
		return gitops.RenderResult{}, fmt.Errorf("load module %s: %w", request.Module, err)
	}
	produce := gitops.ProduceRequest{
		Workspace: workspace, Module: module, Environment: env, AppProject: request.AppProject,
	}
	if request.Service != "" {
		service, err := module.LoadServiceFromName(ctx, request.Service)
		if err != nil {
			return gitops.RenderResult{}, fmt.Errorf("load service %s: %w", request.Service, err)
		}
		produce.Service = service
	}
	return gitops.NewCoordinator().Render(ctx, produce)
}

func (p *planeImpl) PlanGitOpsPublish(ctx context.Context, request *gitops.PublishRequest) (gitops.PublishPlan, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.PublishPlan{}, err
	}
	return gitops.NewCoordinator().PlanPublish(ctx, workspace, request)
}

func (p *planeImpl) PlanGitOpsRollback(ctx context.Context, request *gitops.RollbackRequest) (gitops.RollbackPlan, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.RollbackPlan{}, err
	}
	return gitops.NewCoordinator().PlanRollback(ctx, workspace, request)
}

func (p *planeImpl) ObserveGitOps(ctx context.Context, request *gitops.ObserveRequest) (gitops.ObserveResult, error) {
	if request == nil {
		return gitops.ObserveResult{}, fmt.Errorf("observation request is required")
	}
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.ObserveResult{}, err
	}
	normalized := *request
	normalized.WorkspaceRoot = workspace.Dir()
	env, err := orchestration.SelectEnvironment(workspace, normalized.Environment)
	if err != nil {
		return gitops.ObserveResult{}, fmt.Errorf("select environment %q: %w", normalized.Environment, err)
	}
	normalized.Local = env.IsK3d()
	return gitops.NewCoordinator().Observe(ctx, &normalized)
}

func (p *planeImpl) publishGitOps(ctx context.Context, mutation *gitops.PublishMutation, permit mutationauthority.PreparedPermit) (gitops.PublishResult, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.PublishResult{}, err
	}
	return gitops.NewCoordinator().Publish(ctx, workspace, mutation, permit)
}

func (p *planeImpl) rollbackGitOps(ctx context.Context, mutation *gitops.RollbackMutation, permit mutationauthority.PreparedPermit) (gitops.PublishResult, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.PublishResult{}, err
	}
	return gitops.NewCoordinator().Rollback(ctx, workspace, mutation, permit)
}
