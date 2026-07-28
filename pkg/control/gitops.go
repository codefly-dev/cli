package control

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/gitops"
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
	if request.Service == "" {
		return gitops.RenderModule(ctx, workspace, module, env, request.AppProject, nil)
	}
	service, err := module.LoadServiceFromName(ctx, request.Service)
	if err != nil {
		return gitops.RenderResult{}, fmt.Errorf("load service %s: %w", request.Service, err)
	}
	return gitops.RenderService(ctx, workspace, module, service, env, request.AppProject, false, nil)
}

func (p *planeImpl) PlanGitOpsPublish(ctx context.Context, request gitops.PublishRequest) (gitops.PublishPlan, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.PublishPlan{}, err
	}
	return gitops.PlanPublish(ctx, workspace, request)
}

func (p *planeImpl) PlanGitOpsRollback(ctx context.Context, request gitops.RollbackRequest) (gitops.RollbackPlan, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.RollbackPlan{}, err
	}
	return gitops.PlanRollback(ctx, workspace, request)
}

func (p *planeImpl) ObserveGitOps(ctx context.Context, request gitops.ObserveRequest) (gitops.ObserveResult, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.ObserveResult{}, err
	}
	request.WorkspaceRoot = workspace.Dir()
	return gitops.Observe(ctx, request)
}

func (p *planeImpl) publishGitOps(ctx context.Context, mutation gitops.PublishMutation) (gitops.PublishResult, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.PublishResult{}, err
	}
	return gitops.Publish(ctx, workspace, mutation)
}

func (p *planeImpl) rollbackGitOps(ctx context.Context, mutation gitops.RollbackMutation) (gitops.PublishResult, error) {
	workspace, err := p.workspace(ctx)
	if err != nil {
		return gitops.PublishResult{}, err
	}
	return gitops.Rollback(ctx, workspace, mutation)
}
