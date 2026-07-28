package control

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
)

// Deploy ships a service to an environment. This is a destructive/outward
// action, so under prepared authority it is refused directly — the caller must
// PrepareMutation(Mutation{Kind: MutationDeploy, Payload: req}) then
// ApplyPreparedMutation. Under open (trusted-local) authority it runs directly.
func (p *planeImpl) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	if p.gate.currentMode() == AuthorityPrepared {
		return DeployResult{}, fmt.Errorf("deploy requires preparation under prepared authority; use PrepareMutation then ApplyPreparedMutation")
	}
	return p.runDeploy(ctx, req)
}

func (p *planeImpl) runDeploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	if req.Module != "" && req.Service == "" {
		return DeployResult{}, fmt.Errorf("module-wide deploy is not yet supported via the control plane; specify a service")
	}
	ws, module, service, err := p.loadTarget(ctx, req.Service)
	if err != nil {
		return DeployResult{}, err
	}
	envName := req.Env
	if envName == "" {
		envName = orchestration.LocalEnvironmentName
	}
	env, err := orchestration.SelectEnvironment(ws, envName)
	if err != nil {
		return DeployResult{}, fmt.Errorf("select environment %q: %w", envName, err)
	}
	var deploymentManager *deployments.LocalApplyManager
	if !req.DryRun {
		deploymentManager, err = deployments.NewLocalApplyManager(ctx, ws, env)
		if err != nil {
			return DeployResult{}, err
		}
	}
	// Set unconditionally to the requested value so a prior deploy's dry-run
	// toggle (a package-level global in orchestration) never leaks into this one.
	orchestration.SetDryRun(req.DryRun)

	flow, err := orchestration.NewFlow(ctx, ws, module, service, env, orchestration.DeployMode)
	if err != nil {
		return DeployResult{}, fmt.Errorf("create flow: %w", err)
	}
	if err := flow.InitManagers(ctx); err != nil {
		return DeployResult{}, fmt.Errorf("init managers: %w", err)
	}
	if err := flow.Load(ctx); err != nil {
		return DeployResult{}, fmt.Errorf("load flow: %w", err)
	}
	if deploymentManager != nil {
		flow.WithDeploymentManager(deploymentManager)
	}
	defer stopFlow(flow)

	if err := flow.Deploy(ctx); err != nil {
		result, _ := deployResult(ctx, false, req.DryRun, deploymentManager, ws, module, service)
		return result, fmt.Errorf("deploy %s: %w", req.Service, err)
	}
	result, err := deployResult(ctx, true, req.DryRun, deploymentManager, ws, module, service)
	if err != nil {
		return result, fmt.Errorf("collect deployment evidence for %s: %w", req.Service, err)
	}
	return result, nil
}

func deployResult(
	ctx context.Context,
	succeeded bool,
	dryRun bool,
	manager *deployments.LocalApplyManager,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
) (DeployResult, error) {
	result := DeployResult{Succeeded: succeeded}
	if dryRun {
		digest, err := deployments.RenderedTreeDigest(deployments.KustomizeDir(ctx, workspace, module, service))
		if err != nil {
			return result, err
		}
		result.RenderedTreeDigest = digest
		return result, nil
	}
	target := manager.Target()
	result.Target = &DeployTarget{
		Kind:       target.Kind,
		Kubeconfig: target.Kubeconfig,
		Context:    target.Context,
		Cluster:    target.Cluster,
		APIServer:  target.APIServer,
		K3dCluster: target.K3dCluster,
	}
	var ok bool
	result.RenderedTreeDigest, ok = manager.RenderedDigest(ctx, module, service)
	if !ok {
		return result, fmt.Errorf("rendered-tree digest is unavailable")
	}
	return result, nil
}
