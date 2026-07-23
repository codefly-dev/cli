package control

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/orchestration"
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

// runDeploy builds a DeployMode flow and drives it, mirroring `codefly deploy
// service`. It constructs the flow inline (rather than via buildFlow) because it
// needs the workspace + environment to wire the apply manager. DryRun renders
// manifests without applying (no deployment manager); otherwise the local apply
// manager runs kustomize build | kubectl apply.
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
	// Wire the apply manager AFTER Load (matching the deploy command). Skipped on
	// dry-run so agents only render manifests to disk.
	if !req.DryRun {
		flow.WithDeploymentManager(deployments.NewLocalApplyManager(ctx, ws, env))
	}
	defer stopFlow(flow)

	if err := flow.Deploy(ctx); err != nil {
		return DeployResult{Succeeded: false}, fmt.Errorf("deploy %s: %w", req.Service, err)
	}
	return DeployResult{Succeeded: true}, nil
}
