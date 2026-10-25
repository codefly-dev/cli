package orchestration

import (
	"context"
	"crypto/tls"
	"fmt"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// GRPCTarget is how this run reaches a running service's gRPC endpoint: the
// dial address of its recorded network instance and whether the endpoint
// declares itself secured.
type GRPCTarget struct {
	Address string
	Secure  bool
}

// Dial opens a client connection to the target with the transport its
// declaration asks for, as readiness dials it. The connection is lazy; the
// caller closes it.
func (target GRPCTarget) Dial() (*grpc.ClientConn, error) {
	return grpc.NewClient(target.Address, grpc.WithTransportCredentials(grpcTransport(target.Secure)))
}

// grpcTransport is the transport a gRPC endpoint's own `secured` declaration
// asks for: TLS when it declares it, plaintext otherwise. It is decided by the
// declaration, never guessed from an address: core writes no scheme into a
// gRPC endpoint's address.
func grpcTransport(secure bool) credentials.TransportCredentials {
	if secure {
		return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	return insecure.NewCredentials()
}

// GRPCEndpointTarget resolves a running service's gRPC endpoint as this run
// recorded it now — the host-native instance first, as GetAddressForEndpoint
// answers — with the transport its declaration asks for. An endpoint that
// serves another API is refused by name.
func (flow *Flow) GRPCEndpointTarget(ctx context.Context, module, service, endpoint string) (GRPCTarget, error) {
	if flow == nil || flow.SharedState == nil {
		return GRPCTarget{}, fmt.Errorf("cannot resolve %s/%s/%s: the run records no network state", module, service, endpoint)
	}
	unique := resources.ServiceUnique(module, service)
	mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(unique)
	if !ok {
		return GRPCTarget{}, fmt.Errorf("cannot resolve %s/%s: the run records no network mapping for %s", unique, endpoint, unique)
	}
	for _, mapping := range mappings {
		declared := mapping.GetEndpoint()
		if declared.GetName() != endpoint {
			continue
		}
		if declared.GetApi() != standards.GRPC {
			return GRPCTarget{}, fmt.Errorf("%s/%s serves %q, not gRPC", unique, endpoint, declared.GetApi())
		}
		instance := reachableNetworkInstance(mapping.GetInstances())
		if instance == nil {
			return GRPCTarget{}, fmt.Errorf("%s/%s has no network instance recorded", unique, endpoint)
		}
		address := networkInstanceDialAddress(instance)
		if address == "" {
			return GRPCTarget{}, fmt.Errorf("%s/%s has no address recorded", unique, endpoint)
		}
		return GRPCTarget{Address: address, Secure: resources.IsGRPC(ctx, declared).GetSecured()}, nil
	}
	return GRPCTarget{}, fmt.Errorf("%s declares no endpoint %s in this run", unique, endpoint)
}
