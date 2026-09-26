package orchestration

import (
	"context"

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
