package orchestration

import (
	"context"
	"errors"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/remotenetwork"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
)

// NewEnvironmentRemoteManager builds the remote network manager a render uses
// for a workspace — its DNS contract read the way NewFlow reads it — without
// starting a flow, so a plan-time check and a read-only listing allocate
// exactly the ports the render emits.
func NewEnvironmentRemoteManager(ctx context.Context, workspace *resources.Workspace) (*remotenetwork.RemoteManager, error) {
	manager, err := configurations.NewManager(ctx, workspace)
	if err != nil {
		return nil, err
	}
	localReader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return nil, err
	}
	manager.WithLoader(localReader)
	return remotenetwork.NewRemoteManager(ctx, manager)
}

// PlanDeclaredEndpointPorts refuses a render, before any image is built, when
// a service it renders in-cluster declares spec.deployment.endpoint-ports that
// disagree with the allocation (see remotenetwork.CheckDeclaredEndpointPorts).
// A managed service has no in-cluster workload and is skipped. The deploy
// builder repeats the check on the endpoints its agent reports, so a service
// this plan does not see is still held to it.
func PlanDeclaredEndpointPorts(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, services []*resources.Service) error {
	if workspace == nil || env == nil {
		return nil
	}
	var remote *remotenetwork.RemoteManager
	var problems []error
	for _, service := range services {
		if service == nil {
			continue
		}
		declared, err := remotenetwork.DeclaredEndpointPorts(service)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if len(declared) == 0 {
			continue
		}
		identity, err := service.Identity()
		if err != nil {
			return err
		}
		if _, managed := env.ManagedService(identity.Module, identity.Name); managed {
			continue
		}
		if remote == nil {
			if remote, err = NewEnvironmentRemoteManager(ctx, workspace); err != nil {
				return err
			}
		}
		endpoints, err := service.LoadEndpoints(ctx)
		if err != nil {
			return err
		}
		ports, err := remote.DeployedPorts(ctx, env, identity, endpoints)
		if err != nil {
			return err
		}
		if err = remotenetwork.CheckDeclaredEndpointPorts(service, ports); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
