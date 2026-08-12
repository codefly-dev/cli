package dockerexec

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// Config binds one Codefly Gateway to one already-running container.
type Config struct {
	// ContainerID is the Docker container id or unambiguous name.
	ContainerID string
	// WorkDir is the default workspace root for relative file and command paths.
	WorkDir string
	// AuthorityDir is the widest container path the Gateway may address. Empty
	// defaults to WorkDir. An isolated terminal sandbox may explicitly use "/".
	AuthorityDir string
	// Service is the optional typed route exposed by ListServices.
	Service string
}

// Gateway serves the generated Codefly Gateway contract. Unsupported RPCs
// return the generated Unimplemented status instead of acquiring a second,
// product-owned execution path.
type Gateway struct {
	gatewayv1.UnimplementedGatewayServer
	base base
}

// New proves the Docker CLI and target container are available, then returns a
// typed container Gateway. Infrastructure failure is explicit and immediate.
func New(ctx context.Context, config Config) (*Gateway, error) {
	backend, err := newBase(config)
	if err != nil {
		return nil, err
	}
	if _, lookupErr := exec.LookPath("docker"); lookupErr != nil {
		return nil, fmt.Errorf("docker execution gateway: Docker CLI unavailable: %w", lookupErr)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, stderr, exitCode, probeErr := backend.run(ctx, "", 15, "true")
	if probeErr != nil {
		return nil, fmt.Errorf("docker execution gateway: probe container %q: %w", backend.containerID, probeErr)
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("docker execution gateway: container %q is unavailable: %s", backend.containerID, strings.TrimSpace(stderr))
	}
	return &Gateway{base: backend}, nil
}

func (g *Gateway) ListServices(context.Context, *gatewayv1.ListServicesRequest) (*gatewayv1.ListServicesResponse, error) {
	if g == nil || strings.TrimSpace(g.base.service) == "" {
		return &gatewayv1.ListServicesResponse{}, nil
	}
	return &gatewayv1.ListServicesResponse{Services: []*gatewayv1.ServiceInfo{{Name: g.base.service}}}, nil
}

var _ gatewayv1.GatewayServer = (*Gateway)(nil)
