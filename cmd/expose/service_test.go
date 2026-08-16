package expose

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/routing"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/standards"
)

func TestServiceCommandIsRunE(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("expose service command is not exclusively RunE")
	}
	if ServiceCmd.Hidden {
		t.Fatal("expose service command should no longer be hidden")
	}
}

func TestRoutingFlagDefaultsToARegisteredBackend(t *testing.T) {
	def := ServiceCmd.Flags().Lookup("routing").DefValue
	for _, backend := range routing.Backends() {
		if backend == def {
			return
		}
	}
	t.Fatalf("routing default %q is not a registered backend %v", def, routing.Backends())
}

// mTLS emits a policy whose selector (app=<service>) is a convention we cannot
// verify against a workload here, so it must be opt-in, not on by default.
func TestMTLSDefaultsOff(t *testing.T) {
	if ServiceCmd.Flags().Lookup("mtls").DefValue != "false" {
		t.Fatal("--mtls should default to false")
	}
}

// A named sibling of the same API, and a second API that shares a canonical
// port, must NOT reuse standards.Port — the network layer binds them to a
// stable endpoint-specific port, and a route to standards.Port would miss.
func TestInClusterPortsMatchesCanonicalVsNamedRule(t *testing.T) {
	ctx := context.Background()

	// rest and http both canonicalize to 8080; rest (higher priority) owns it.
	restHTTP := []*resources.Endpoint{
		{Name: "rest", API: standards.REST},
		{Name: "http", API: standards.HTTP},
	}
	ports := inClusterPorts(ctx, "platform", "accounts", restHTTP)
	if ports["rest"] != standards.Port(standards.REST) {
		t.Fatalf("rest should own canonical 8080, got %d", ports["rest"])
	}
	wantHTTP := network.ToNamedPort(ctx, "", "platform", "accounts", "http", standards.HTTP, network.PortModeHost)
	if ports["http"] != wantHTTP {
		t.Fatalf("http should get named port %d, got %d", wantHTTP, ports["http"])
	}
	if ports["http"] == standards.Port(standards.HTTP) {
		t.Fatal("http must not reuse the canonical 8080 owned by rest")
	}

	// Two gRPC endpoints: the conventional one (name==api) owns 9090, the
	// named sibling gets an endpoint-specific port.
	siblings := []*resources.Endpoint{
		{Name: "grpc", API: standards.GRPC},
		{Name: "grpc-admin", API: standards.GRPC},
	}
	ports = inClusterPorts(ctx, "platform", "accounts", siblings)
	if ports["grpc"] != standards.Port(standards.GRPC) {
		t.Fatalf("conventional grpc should own 9090, got %d", ports["grpc"])
	}
	wantAdmin := network.ToNamedPort(ctx, "", "platform", "accounts", "grpc-admin", standards.GRPC, network.PortModeHost)
	if ports["grpc-admin"] != wantAdmin {
		t.Fatalf("grpc-admin should get named port %d, got %d", wantAdmin, ports["grpc-admin"])
	}
}

// A private endpoint can own the canonical port, forcing a public sibling onto
// a named port; the port computation must consider all endpoints, not just
// public ones.
func TestInClusterPortsAccountsForPrivateOwner(t *testing.T) {
	ctx := context.Background()
	endpoints := []*resources.Endpoint{
		{Name: "grpc", API: standards.GRPC, Visibility: resources.VisibilityPrivate},
		{Name: "grpc-public", API: standards.GRPC, Visibility: resources.VisibilityPublic},
	}
	ports := inClusterPorts(ctx, "platform", "accounts", endpoints)
	if ports["grpc"] != standards.Port(standards.GRPC) {
		t.Fatalf("conventional private grpc should own 9090, got %d", ports["grpc"])
	}
	if ports["grpc-public"] == standards.Port(standards.GRPC) {
		t.Fatal("public sibling must not reuse the canonical port owned by the private conventional endpoint")
	}
}

func TestExposedEndpointsSelectsRoutablePublicEndpointsWithHostsAndPrefix(t *testing.T) {
	ctx := context.Background()
	service := &resources.Service{
		Name: "accounts",
		Endpoints: []*resources.Endpoint{
			{Name: "grpc", API: standards.GRPC, Visibility: resources.VisibilityPublic},
			{Name: "rest", API: standards.REST, Visibility: resources.VisibilityPublic},
			{Name: "internal", API: standards.GRPC, Visibility: resources.VisibilityPrivate},
			{Name: "raw", API: standards.TCP, Visibility: resources.VisibilityPublic},
		},
	}
	env := &resources.Environment{
		Ingress: []resources.EnvironmentIngressRoute{
			{Service: "accounts", Hosts: []string{"api.acme.dev"}},
		},
	}
	got := exposedEndpoints(ctx, "platform", service, env, nil, "acme.accounts.v1")

	byName := map[string]routing.ExposedEndpoint{}
	for _, ep := range got {
		byName[ep.Name] = ep
	}
	if len(got) != 2 {
		t.Fatalf("expected grpc+rest only, got %+v", got)
	}
	if byName["grpc"].Prefix != "acme.accounts.v1" {
		t.Fatalf("grpc endpoint should carry the proto-package prefix, got %q", byName["grpc"].Prefix)
	}
	if byName["rest"].Prefix != "" {
		t.Fatalf("HTTP endpoint must not carry the proto-package prefix, got %q", byName["rest"].Prefix)
	}
	for _, name := range []string{"grpc", "rest"} {
		if len(byName[name].Hosts) != 1 || byName[name].Hosts[0] != "api.acme.dev" {
			t.Fatalf("%s should inherit the service-wide ingress host, got %v", name, byName[name].Hosts)
		}
	}
}

// Ingress routes that name a specific endpoint must bind hosts only to that
// endpoint; a service-wide route (empty Endpoint) applies to all. Matching
// works by bare name or module/service unique.
func TestIngressHostsHonorsEndpointBindingAndUniqueForm(t *testing.T) {
	env := &resources.Environment{
		Ingress: []resources.EnvironmentIngressRoute{
			{Service: "accounts", Endpoint: "grpc", Hosts: []string{"grpc.acme.dev"}},
			{Service: "platform/accounts", Endpoint: "rest", Hosts: []string{"rest.acme.dev"}},
			{Service: "accounts", Hosts: []string{"all.acme.dev", "all.acme.dev"}},
			{Service: "other", Hosts: []string{"nope.acme.dev"}},
		},
	}

	grpc := ingressHosts(env, "platform", "accounts", "grpc")
	if len(grpc) != 2 || grpc[0] != "grpc.acme.dev" || grpc[1] != "all.acme.dev" {
		t.Fatalf("grpc hosts = %v, want [grpc.acme.dev all.acme.dev]", grpc)
	}
	rest := ingressHosts(env, "platform", "accounts", "rest")
	if len(rest) != 2 || rest[0] != "rest.acme.dev" || rest[1] != "all.acme.dev" {
		t.Fatalf("rest hosts = %v, want [rest.acme.dev all.acme.dev] (unique-form route + service-wide)", rest)
	}
	// The endpoint-specific host for grpc must not leak onto rest.
	for _, h := range rest {
		if h == "grpc.acme.dev" {
			t.Fatal("grpc-specific host leaked onto rest endpoint")
		}
	}
}
