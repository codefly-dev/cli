package remotenetwork

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
)

// selectiveDNSManager declares DNS for the named endpoints only.
type selectiveDNSManager map[string]*basev0.DNS

func (m selectiveDNSManager) GetDNS(_ context.Context, _ *resources.ServiceIdentity, endpoint string) (*basev0.DNS, error) {
	if dns, ok := m[endpoint]; ok {
		return dns, nil
	}
	return nil, fmt.Errorf("no DNS for %s", endpoint)
}

func renderedMappings(mappings []*basev0.NetworkMapping) string {
	var lines []string
	for _, mapping := range mappings {
		for _, instance := range mapping.Instances {
			lines = append(lines, fmt.Sprintf("%s %s %s %d", mapping.Endpoint.Name, instance.GetAccess().GetKind(), instance.GetAddress(), instance.GetPort()))
		}
	}
	return strings.Join(lines, "\n")
}

// TestRemoteManagerPinsMultiEndpointPorts pins every address and port a
// multi-endpoint service renders with: canonical owners, named siblings, an API
// losing a shared canonical port, a declared-DNS endpoint and an external one.
// The expected text was captured from the render before the allocation moved to
// core's network.DeployedEndpointPorts; it must never change.
func TestRemoteManagerPinsMultiEndpointPorts(t *testing.T) {
	ctx := context.Background()
	manager, err := NewRemoteManager(ctx, selectiveDNSManager{
		"public": {Host: "api.example.com", Port: 443, Secured: true},
		"admin":  {Host: "admin.internal.example", Port: 7443},
	})
	require.NoError(t, err)
	environment := &environments.Environment{Name: "staging", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "acme", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "saas", Name: "accounts"}
	endpoint := func(name, api string) *basev0.Endpoint {
		return &basev0.Endpoint{Module: "saas", Service: "accounts", Name: name, Api: api}
	}
	public := endpoint("public", standards.REST)
	public.Location = resources.LocationExternal
	endpoints := []*basev0.Endpoint{
		endpoint("authority", standards.GRPC),
		endpoint("connect", standards.CONNECT),
		endpoint("grpc", standards.GRPC),
		endpoint("rest", standards.REST),
		endpoint("http", standards.HTTP),
		endpoint("mcp", standards.MCP),
		endpoint("tcp", standards.TCP),
		endpoint("admin", standards.HTTP),
		public,
	}

	mappings, err := manager.GenerateNetworkMappings(ctx, environment, workspace, service, endpoints)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(pinnedMultiEndpointMappings), renderedMappings(mappings))
}

const pinnedMultiEndpointMappings = `
authority public accounts.platform.svc.cluster.local:52893 52893
authority container accounts.platform.svc.cluster.local:52893 52893
connect public http://accounts.platform.svc.cluster.local:8081 8081
connect container http://accounts.platform.svc.cluster.local:8081 8081
grpc public accounts.platform.svc.cluster.local:9090 9090
grpc container accounts.platform.svc.cluster.local:9090 9090
rest public http://accounts.platform.svc.cluster.local:8080 8080
rest container http://accounts.platform.svc.cluster.local:8080 8080
http public http://accounts.platform.svc.cluster.local:4871 4871
http container http://accounts.platform.svc.cluster.local:4871 4871
mcp public http://accounts.platform.svc.cluster.local:8082 8082
mcp container http://accounts.platform.svc.cluster.local:8082 8082
tcp public accounts.platform.svc.cluster.local:80 80
tcp container accounts.platform.svc.cluster.local:80 80
admin public http://admin.internal.example:7443 7443
admin container http://admin.internal.example:7443 7443
public public https://api.example.com:443 443
`
