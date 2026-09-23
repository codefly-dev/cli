package remotenetwork

import (
	"context"
	"fmt"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corenetwork "github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

type remoteDNSManager struct {
	dns *basev0.DNS
}

func TestKubernetesServicePreservesDeclaredTLS(t *testing.T) {
	endpoint := &basev0.Endpoint{
		Api:        standards.HTTP,
		ApiDetails: &basev0.API{Value: &basev0.API_Http{Http: &basev0.HttpAPI{Secured: true}}},
	}
	instance := (&RemoteManager{}).KubernetesService(&resources.ServiceIdentity{Name: "model"}, endpoint, "namespace", 8443)
	require.Equal(t, "https://model.namespace.svc.cluster.local:8443", instance.Address)
}

func TestPairingRejectsPortsBeforeStartingProcesses(t *testing.T) {
	for _, invalid := range []uint32{65536, 1<<32 - 1} {
		for _, side := range []string{"local", "remote"} {
			local := corenetwork.NativeInstance(resources.NewNetworkInstance("localhost", 12345))
			remote := corenetwork.ContainerInstance(resources.NewNetworkInstance("api.product.svc.cluster.local", 8080))
			if side == "local" {
				local.Port = invalid
			} else {
				remote.Port = invalid
			}
			pairing := &Pairing{Local: &basev0.NetworkMapping{Instances: []*basev0.NetworkInstance{local}}, Remote: &basev0.NetworkMapping{Instances: []*basev0.NetworkInstance{remote}}}
			err := (&RemoteManager{}).StartPairing(t.Context(), nil, nil, &resources.ServiceIdentity{Name: "api"}, pairing, noopOutput{})
			require.ErrorContains(t, err, side+" port")
		}
	}
}

func (manager remoteDNSManager) GetDNS(context.Context, *resources.ServiceIdentity, string) (*basev0.DNS, error) {
	return manager.dns, nil
}

func TestRemoteManagerUsesDeclaredEnvironmentNamespace(t *testing.T) {
	manager := &RemoteManager{}
	environment := &environments.Environment{Name: "production", Namespace: "platform"}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}

	for _, layout := range []resources.LayoutKind{resources.LayoutKindModules, resources.LayoutKindFlat} {
		workspace := &resources.Workspace{Name: "mind", Layout: layout}
		namespace, err := manager.GetNamespace(context.Background(), environment, workspace, service)
		if err != nil {
			t.Fatal(err)
		}
		if namespace != "platform" {
			t.Fatalf("layout %s namespace = %q, want platform", layout, namespace)
		}
	}
}

func TestRemoteManagerSynthesizesLegacyNamespaceWhenUndeclared(t *testing.T) {
	manager := &RemoteManager{}
	environment := &environments.Environment{Name: "local"}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}

	tests := []struct {
		layout resources.LayoutKind
		want   string
	}{
		{layout: resources.LayoutKindModules, want: "mind-users-local"},
		{layout: resources.LayoutKindFlat, want: "mind-local"},
	}
	for _, test := range tests {
		workspace := &resources.Workspace{Name: "mind", Layout: test.layout}
		namespace, err := manager.GetNamespace(context.Background(), environment, workspace, service)
		if err != nil {
			t.Fatal(err)
		}
		if namespace != test.want {
			t.Fatalf("layout %s namespace = %q, want %q", test.layout, namespace, test.want)
		}
	}
}

func TestRemoteManagerUsesDeclaredInternalDNSForContainerAccess(t *testing.T) {
	manager, err := NewRemoteManager(context.Background(), remoteDNSManager{
		dns: &basev0.DNS{
			Host:    "store.platform.svc.cluster.local",
			Port:    5432,
			Secured: false,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{Name: "aws", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "users", Name: "store"}
	endpoint := &basev0.Endpoint{
		Module:  "users",
		Service: "store",
		Name:    "tcp",
		Api:     standards.TCP,
	}

	mappings, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 1 || len(mappings[0].Instances) != 2 {
		t.Fatalf("mappings = %#v, want one mapping with public and container instances", mappings)
	}
	for _, access := range []*basev0.NetworkAccess{
		resources.NewPublicNetworkAccess(),
		resources.NewContainerNetworkAccess(),
	} {
		instance := resources.FilterNetworkInstance(context.Background(), mappings[0].Instances, access)
		if instance == nil {
			t.Fatalf("missing %q access instance", access.GetKind())
		}
		if instance.GetHostname() != "store.platform.svc.cluster.local" || instance.GetPort() != 5432 {
			t.Fatalf("%s instance = %s:%d, want store.platform.svc.cluster.local:5432", access.GetKind(), instance.GetHostname(), instance.GetPort())
		}
	}
}

func TestRemoteManagerSynthesizesInternalDNSWhenUndeclared(t *testing.T) {
	manager, err := NewRemoteManager(context.Background(), remoteDNSManager{})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{Name: "aws", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
	endpoint := &basev0.Endpoint{
		Module:  "users",
		Service: "accounts",
		Name:    "grpc",
		Api:     standards.GRPC,
	}

	mappings, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 1 || len(mappings[0].Instances) != 2 {
		t.Fatalf("mappings = %#v, want one mapping with public and container instances", mappings)
	}
	for _, access := range []*basev0.NetworkAccess{
		resources.NewPublicNetworkAccess(),
		resources.NewContainerNetworkAccess(),
	} {
		instance := resources.FilterNetworkInstance(context.Background(), mappings[0].Instances, access)
		if instance == nil {
			t.Fatalf("missing %q access instance", access.GetKind())
		}
		if instance.GetHostname() != "accounts.platform.svc.cluster.local" || instance.GetPort() != uint32(standards.Port(standards.GRPC)) {
			t.Fatalf("%s instance = %s:%d, want accounts.platform.svc.cluster.local:%d", access.GetKind(), instance.GetHostname(), instance.GetPort(), standards.Port(standards.GRPC))
		}
	}
}

type noopOutput struct{}

func (noopOutput) ProcessWithSource(*wool.Identifier, *wool.Log) {}

func TestRemoteManagerStartPairingAcceptsTwoInstanceRemoteMapping(t *testing.T) {
	manager, err := NewRemoteManager(context.Background(), remoteDNSManager{})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{Name: "aws", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
	endpoint := &basev0.Endpoint{Module: "users", Service: "accounts", Name: "grpc", Api: standards.GRPC}

	remotes, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if len(remotes[0].Instances) != 2 {
		t.Fatalf("expected a two-instance synthesized remote mapping, got %#v", remotes[0].Instances)
	}
	local := &basev0.NetworkMapping{
		Endpoint:  endpoint,
		Instances: []*basev0.NetworkInstance{corenetwork.NativeInstance(resources.NewNetworkInstance("localhost", 12345))},
	}
	pairing := &Pairing{Local: local, Remote: remotes[0]}

	// Cancel before StartPairing so the kubectl goroutines return without
	// spawning a real process; this test only exercises the instance-selection
	// guard, which runs before any goroutine is launched.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.StartPairing(ctx, environment, workspace, service, pairing, noopOutput{}); err != nil {
		t.Fatalf("StartPairing rejected a two-instance remote mapping: %v", err)
	}
	manager.Stop()
}

func TestRemoteManagerAssignsSameAPIEndpointsIndependentPorts(t *testing.T) {
	manager, err := NewRemoteManager(context.Background(), remoteDNSManager{})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{Name: "aws", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "saas", Name: "accounts"}
	endpoints := []*basev0.Endpoint{
		{Module: "saas", Service: "accounts", Name: "grpc", Api: standards.GRPC},
		{Module: "saas", Service: "accounts", Name: "usage", Api: standards.GRPC},
	}

	mappings, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, service, endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 {
		t.Fatalf("mappings = %d, want 2", len(mappings))
	}
	grpcPort := mappings[0].Instances[0].GetPort()
	usagePort := mappings[1].Instances[0].GetPort()
	if grpcPort != uint32(standards.Port(standards.GRPC)) {
		t.Fatalf("default grpc port = %d, want %d", grpcPort, standards.Port(standards.GRPC))
	}
	if usagePort == grpcPort {
		t.Fatalf("named usage endpoint reused grpc port %d", grpcPort)
	}
}

func TestRemoteManagerAssignsCollidingAPIsIndependentStablePorts(t *testing.T) {
	manager, err := NewRemoteManager(context.Background(), remoteDNSManager{})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{Name: "aws", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "web", Name: "gateway"}
	rest := &basev0.Endpoint{Module: "web", Service: "gateway", Name: standards.REST, Api: standards.REST}
	http := &basev0.Endpoint{Module: "web", Service: "gateway", Name: standards.HTTP, Api: standards.HTTP}

	forward, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{rest, http})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{http, rest})
	if err != nil {
		t.Fatal(err)
	}
	ports := func(mappings []*basev0.NetworkMapping) map[string]uint32 {
		result := make(map[string]uint32, len(mappings))
		for _, mapping := range mappings {
			result[mapping.Endpoint.Name] = mapping.Instances[0].Port
		}
		return result
	}
	forwardPorts := ports(forward)
	require.NotEqual(t, forwardPorts[standards.REST], forwardPorts[standards.HTTP])
	require.Equal(t, uint32(standards.Port(standards.REST)), forwardPorts[standards.REST])
	require.Equal(t, forwardPorts, ports(reverse))
}

type erroringDNSManager struct{}

func (erroringDNSManager) GetDNS(context.Context, *resources.ServiceIdentity, string) (*basev0.DNS, error) {
	return nil, fmt.Errorf("no DNS found")
}

func TestRemoteManagerDerivesExternalHostFromAppHostSuffix(t *testing.T) {
	// No local dns.codefly.yaml entry (the manager reports a miss), but the
	// environment carries an app host suffix — the external host is derived from
	// declared config, so the render is value-free.
	for name, manager := range map[string]corenetwork.DNSManager{
		"nil-miss": remoteDNSManager{},
		"err-miss": erroringDNSManager{},
	} {
		t.Run(name, func(t *testing.T) {
			rm, err := NewRemoteManager(context.Background(), manager)
			if err != nil {
				t.Fatal(err)
			}
			environment := &environments.Environment{
				Name:      "azure",
				Namespace: "platform",
				DNS:       &environments.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"},
			}
			workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
			service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
			endpoint := &basev0.Endpoint{Module: "users", Service: "accounts", Name: "api", Api: standards.REST, Location: resources.LocationExternal}

			mappings, err := rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
			if err != nil {
				t.Fatalf("derive external host: %v", err)
			}
			if len(mappings) != 1 || len(mappings[0].Instances) != 1 {
				t.Fatalf("mappings = %#v", mappings)
			}
			got := mappings[0].Instances[0]
			if got.Hostname != "accounts-users.staging.eastus2.azure.example.com" {
				t.Errorf("derived host = %q", got.Hostname)
			}
			if got.Port != 443 {
				t.Errorf("derived port = %d, want 443", got.Port)
			}
		})
	}
}

func TestRemoteManagerExternalHostPrefersDeclaredDNS(t *testing.T) {
	// A declared dns.codefly.yaml entry wins over the derived suffix host.
	rm, err := NewRemoteManager(context.Background(), remoteDNSManager{
		dns: &basev0.DNS{Host: "api.declared.example.com", Port: 8443, Secured: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{
		Name:      "azure",
		Namespace: "platform",
		DNS:       &environments.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"},
	}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
	endpoint := &basev0.Endpoint{Module: "users", Service: "accounts", Name: "api", Api: standards.REST, Location: resources.LocationExternal}

	mappings, err := rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if got := mappings[0].Instances[0].Hostname; got != "api.declared.example.com" {
		t.Errorf("host = %q, want the declared DNS to win", got)
	}
}

// assertInClusterMapping checks that a mapping resolves both public and
// container access to the synthesized in-cluster Service address.
func assertInClusterMapping(t *testing.T, mappings []*basev0.NetworkMapping, host string, port uint32) {
	t.Helper()
	require.Len(t, mappings, 1)
	require.Len(t, mappings[0].Instances, 2, "want public and container instances")
	for _, access := range []*basev0.NetworkAccess{
		resources.NewPublicNetworkAccess(),
		resources.NewContainerNetworkAccess(),
	} {
		instance := resources.FilterNetworkInstance(context.Background(), mappings[0].Instances, access)
		require.NotNil(t, instance, "missing %q access instance", access.GetKind())
		require.Equal(t, host, instance.GetHostname(), "%s hostname", access.GetKind())
		require.Equal(t, port, instance.GetPort(), "%s port", access.GetKind())
	}
}

func TestRemoteManagerSynthesizesInClusterAddressForRenderedExternalWithoutDNS(t *testing.T) {
	// Neither a declared DNS entry nor an app host suffix, but the service is
	// one this render emits as an in-cluster workload: its ClusterIP Service is
	// the address that exists, so the mapping falls through to the synthesized
	// in-cluster form instead of failing the render on a missing public host.
	for name, manager := range map[string]corenetwork.DNSManager{
		"nil-miss": remoteDNSManager{},
		"err-miss": erroringDNSManager{},
	} {
		t.Run(name, func(t *testing.T) {
			rm, err := NewRemoteManager(context.Background(), manager)
			require.NoError(t, err)
			environment := &environments.Environment{Name: "azure", Namespace: "platform"}
			workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
			service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
			endpoint := &basev0.Endpoint{Module: "users", Service: "accounts", Name: "api", Api: standards.REST, Location: resources.LocationExternal}

			mappings, err := rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
			require.NoError(t, err)
			assertInClusterMapping(t, mappings, "accounts.platform.svc.cluster.local", uint32(standards.Port(standards.REST)))
		})
	}
}

func TestRemoteManagerManagedServiceExternalFailsWithoutSuffixOrDNS(t *testing.T) {
	// A managed service is not rendered as an in-cluster workload — its bundle
	// is bootstrap-only — so there is no ClusterIP to fall back on. With neither
	// a declared DNS entry nor an app host suffix, nothing exists to route to and
	// the hard failure stands.
	rm, err := NewRemoteManager(context.Background(), erroringDNSManager{})
	require.NoError(t, err)
	environment := &environments.Environment{
		Name:            "azure",
		Namespace:       "platform",
		ManagedServices: map[string]environments.EnvironmentManagedService{"accounts": {Kind: "external", ExternalName: "accounts.example.internal"}},
	}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
	endpoint := &basev0.Endpoint{Module: "users", Service: "accounts", Name: "api", Api: standards.REST, Location: resources.LocationExternal}

	_, err = rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	require.Error(t, err, "expected a failure when a managed service declares no DNS and no app host suffix")
}

func TestRemoteManagerDerivesGRPCExternalHost(t *testing.T) {
	// A gRPC external is public-routable over TLS on 443, so it derives from the
	// app host suffix just like an HTTP endpoint — even though gRPC is not
	// HTTP-based. This pins the gate to "supported, non-TCP", not "HTTP-based":
	// narrowing it back to IsHTTPBasedAPI would silently drop gRPC externals.
	rm, err := NewRemoteManager(context.Background(), erroringDNSManager{})
	if err != nil {
		t.Fatal(err)
	}
	environment := &environments.Environment{
		Name:      "azure",
		Namespace: "platform",
		DNS:       &environments.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"},
	}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "users", Name: "accounts"}
	endpoint := &basev0.Endpoint{Module: "users", Service: "accounts", Name: "grpc", Api: standards.GRPC, Location: resources.LocationExternal}

	mappings, err := rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	if err != nil {
		t.Fatalf("derive gRPC external host: %v", err)
	}
	if len(mappings) != 1 || len(mappings[0].Instances) != 1 {
		t.Fatalf("mappings = %#v", mappings)
	}
	got := mappings[0].Instances[0]
	if got.Hostname != "accounts-users.staging.eastus2.azure.example.com" || got.Port != 443 {
		t.Errorf("derived gRPC instance = %q:%d, want accounts-users.staging.eastus2.azure.example.com:443", got.Hostname, got.Port)
	}
}

func TestRemoteManagerRenderedTCPExternalSynthesizesInClusterAddressNotAppEdgeHost(t *testing.T) {
	// A raw TCP external (a postgres agent scaffolds its tcp endpoint with the
	// deprecated `visibility: external`) has no public app-edge host: deriving one
	// from the suffix would emit a bogus, non-resolving TLS host. The suffix must
	// be ignored for TCP. The service is still rendered in-cluster by this flow,
	// so the mapping is the synthesized ClusterIP Service on the canonical port —
	// not a failure, and not the app-edge host.
	rm, err := NewRemoteManager(context.Background(), erroringDNSManager{})
	require.NoError(t, err)
	environment := &environments.Environment{
		Name:      "azure",
		Namespace: "platform",
		DNS:       &environments.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"},
	}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "saas", Name: "store"}
	endpoint := &basev0.Endpoint{Module: "saas", Service: "store", Name: "tcp", Api: standards.TCP, Location: resources.LocationExternal}

	mappings, err := rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	require.NoError(t, err)
	assertInClusterMapping(t, mappings, "store.platform.svc.cluster.local", uint32(standards.Port(standards.TCP)))
	for _, instance := range mappings[0].Instances {
		require.NotContains(t, instance.GetHostname(), "staging.eastus2.azure.example.com", "a TCP external must not derive an app-edge host from the suffix")
	}
}

func TestRemoteManagerManagedTCPExternalStillRequiresDeclaredDNS(t *testing.T) {
	// The managed-database case the TCP rule exists for: the environment declares
	// the service as managed, so the render emits no in-cluster workload and the
	// endpoint still requires a declared DNS entry — absent one, the hard failure
	// stands even though an app host suffix is declared.
	rm, err := NewRemoteManager(context.Background(), erroringDNSManager{})
	require.NoError(t, err)
	environment := &environments.Environment{
		Name:            "azure",
		Namespace:       "platform",
		DNS:             &environments.EnvironmentDNS{AppHostSuffix: "staging.eastus2.azure.example.com"},
		ManagedServices: map[string]environments.EnvironmentManagedService{"store": {Kind: "postgres", ExternalName: "store.example.internal", Port: 5432}},
	}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "saas", Name: "store"}
	endpoint := &basev0.Endpoint{Module: "saas", Service: "store", Name: "tcp", Api: standards.TCP, Location: resources.LocationExternal}

	_, err = rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, []*basev0.Endpoint{endpoint})
	require.Error(t, err, "expected a failure: a managed TCP external must not derive an app-edge host or an in-cluster address")
}

func TestRemoteManagerRenderedExternalWithoutDNSSharesCanonicalPortAllocation(t *testing.T) {
	// An external endpoint that falls through to the in-cluster form competes for
	// the canonical port like any internal endpoint: a sibling of the same API
	// must not collide with it.
	rm, err := NewRemoteManager(context.Background(), erroringDNSManager{})
	require.NoError(t, err)
	environment := &environments.Environment{Name: "azure", Namespace: "platform"}
	workspace := &resources.Workspace{Name: "mind", Layout: resources.LayoutKindModules}
	service := &resources.ServiceIdentity{Module: "saas", Name: "accounts"}
	endpoints := []*basev0.Endpoint{
		{Module: "saas", Service: "accounts", Name: "grpc", Api: standards.GRPC, Location: resources.LocationExternal},
		{Module: "saas", Service: "accounts", Name: "usage", Api: standards.GRPC},
	}

	mappings, err := rm.GenerateNetworkMappings(context.Background(), environment, workspace, service, endpoints)
	require.NoError(t, err)
	require.Len(t, mappings, 2)
	require.Equal(t, uint32(standards.Port(standards.GRPC)), mappings[0].Instances[0].GetPort())
	require.NotEqual(t, mappings[0].Instances[0].GetPort(), mappings[1].Instances[0].GetPort())
}

// A workspace composing several modules gives each module its own namespace,
// "<namespace>-<module>", and the address synthesized for a service names the
// namespace of the module that owns it — so a consumer in module A is handed
// "store.<ns>-B.svc.cluster.local" for a provider in module B, never its own.
func TestRemoteManagerScopesNamespacePerModuleWhenWorkspaceComposesSeveral(t *testing.T) {
	manager, err := NewRemoteManager(context.Background(), remoteDNSManager{})
	require.NoError(t, err)
	environment := &environments.Environment{Name: "staging", Namespace: "platform"}
	workspace := &resources.Workspace{
		Name:   "acme",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "saas"}, {Name: "documents"}, {Name: "runtime"},
		},
	}

	for module, want := range map[string]string{
		"saas":      "platform-saas",
		"documents": "platform-documents",
		"runtime":   "platform-runtime",
	} {
		namespace, err := manager.GetNamespace(context.Background(), environment, workspace, &resources.ServiceIdentity{Module: module, Name: "store"})
		require.NoError(t, err)
		require.Equal(t, want, namespace, "module %s", module)
	}

	// The provider's own mappings — the ones its consumers are handed.
	provider := &resources.ServiceIdentity{Module: "documents", Name: "store"}
	endpoint := &basev0.Endpoint{Module: "documents", Service: "store", Name: "grpc", Api: standards.GRPC}
	mappings, err := manager.GenerateNetworkMappings(context.Background(), environment, workspace, provider, []*basev0.Endpoint{endpoint})
	require.NoError(t, err)
	assertInClusterMapping(t, mappings, "store.platform-documents.svc.cluster.local", uint32(standards.Port(standards.GRPC)))

	// A single-module workspace keeps the environment namespace as is.
	single := &resources.Workspace{Name: "acme", Layout: resources.LayoutKindModules, Modules: []*resources.ModuleReference{{Name: "saas"}}}
	namespace, err := manager.GetNamespace(context.Background(), environment, single, provider)
	require.NoError(t, err)
	require.Equal(t, "platform", namespace)
}
