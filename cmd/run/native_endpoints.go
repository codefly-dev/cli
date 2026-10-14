package run

import (
	"context"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
)

// deliverEndpoints posts the endpoints a delivered presence names, resolved by
// this run (Flow.EndpointDelivery: hostRoutes, visibility and hostRouteCarrier,
// as the gateway's startup carriers are derived) and bound to the generation
// the host just settled, signed and delivered as the presence was. The gateway
// reads them from the host; no directory or file carries an endpoint. A
// tombstone delivers none: the host withdraws the binding's endpoints with
// its presence. Called under the cluster delivery lock.
func (cluster *nativeCluster) deliverEndpoints(ctx context.Context, deliverer *gitops.NativeDeliverer, presence *solutionhost.SolutionHostBinding) error {
	if presence.Removed {
		return nil
	}
	raw, err := cluster.flow.EndpointDelivery(ctx, presence, resources.NewNativeNetworkAccess())
	if err != nil {
		return err
	}
	receipt, err := deliverer.DeliverEndpoints(ctx, presence, raw)
	if err != nil {
		return err
	}
	wool.Get(ctx).In("nativeCluster.deliver").Info("delivered", wool.Field("kind", receipt.Kind), wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation))
	return nil
}
