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
	var deploymentManager deployments.Manager
	var evidenceProvider deployments.EvidenceProvider
	if req.DryRun {
		manager := deployments.NewRenderManager(ws, env)
		deploymentManager = manager
		evidenceProvider = manager
	} else {
		manager, managerErr := deployments.NewLocalApplyManager(ctx, ws, env)
		if managerErr != nil {
			return DeployResult{}, managerErr
		}
		deploymentManager = manager
		evidenceProvider = manager
	}

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
	flow.WithDeploymentManager(deploymentManager)
	defer stopFlow(flow)

	if err := flow.Deploy(ctx); err != nil {
		result, _ := deployResult(false, evidenceProvider)
		return result, fmt.Errorf("deploy %s: %w", req.Service, err)
	}
	result, err := deployResult(true, evidenceProvider)
	if err != nil {
		return result, fmt.Errorf("collect deployment evidence for %s: %w", req.Service, err)
	}
	return result, nil
}

func deployResult(succeeded bool, provider deployments.EvidenceProvider) (DeployResult, error) {
	result := DeployResult{Succeeded: succeeded}
	evidence := provider.Evidence()
	if evidence.Target != nil {
		target := evidence.Target
		result.Target = &DeployTarget{
			Kind:            target.Kind,
			Kubeconfig:      target.Kubeconfig,
			Context:         target.Context,
			Cluster:         target.Cluster,
			APIServer:       target.APIServer,
			K3dCluster:      target.K3dCluster,
			ClusterIdentity: target.ClusterIdentity,
		}
	}
	for _, tree := range evidence.RenderedTrees {
		result.RenderedTrees = append(result.RenderedTrees, RenderedTree{
			Module:    tree.Module,
			Service:   tree.Service,
			Digest:    tree.Digest,
			Manifests: tree.Manifests,
		})
	}
	if len(result.RenderedTrees) == 0 {
		return result, fmt.Errorf("rendered-tree evidence is unavailable")
	}
	return result, nil
}
