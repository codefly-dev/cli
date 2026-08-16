package expose

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/routing"
	"github.com/codefly-dev/core/resources"
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

func TestPublicEndpointsSelectsOnlyRoutablePublicEndpoints(t *testing.T) {
	service := &resources.Service{
		Endpoints: []*resources.Endpoint{
			{Name: "grpc", API: "grpc", Visibility: resources.VisibilityPublic},
			{Name: "rest", API: "rest", Visibility: resources.VisibilityPublic},
			{Name: "internal", API: "grpc", Visibility: resources.VisibilityPrivate},
			{Name: "raw", API: "tcp", Visibility: resources.VisibilityPublic},
		},
	}
	got := publicEndpoints(service)
	if len(got) != 2 {
		t.Fatalf("public endpoints = %+v, want grpc+rest only", got)
	}
	byName := map[string]routing.ExposedEndpoint{}
	for _, ep := range got {
		byName[ep.Name] = ep
	}
	if byName["grpc"].Port != 9090 || byName["rest"].Port != 8080 {
		t.Fatalf("unexpected canonical ports: %+v", got)
	}
}

func TestIngressHostsMatchesServiceAndDedupes(t *testing.T) {
	env := &resources.Environment{
		Ingress: []resources.EnvironmentIngressRoute{
			{Service: "accounts", Hosts: []string{"api.acme.dev", "api.acme.dev"}},
			{Service: "accounts", Hosts: []string{"accounts.acme.dev"}},
			{Service: "web", Hosts: []string{"acme.dev"}},
		},
	}
	got := ingressHosts(env, "accounts")
	want := []string{"api.acme.dev", "accounts.acme.dev"}
	if len(got) != len(want) {
		t.Fatalf("hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hosts = %v, want %v", got, want)
		}
	}
}
