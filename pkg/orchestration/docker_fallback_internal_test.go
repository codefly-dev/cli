package orchestration

import (
	"context"
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

func fakeManager(module, name, runtimeContext string, backends ...agentv0.Backend_Type) *Manager {
	supported := make([]*agentv0.Backend, 0, len(backends))
	for _, t := range backends {
		supported = append(supported, &agentv0.Backend{Type: t})
	}
	instance := &services.Instance{
		Identity: &resources.ServiceIdentity{Module: module, Name: name},
		Info:     &agentv0.AgentInformation{SupportedBackends: supported},
	}
	return &Manager{Runner: &Runner{instance: instance, runtimeContext: runtimeContext}}
}

func flowWith(docker DockerStatus, managers ...IManager) *Flow {
	return &Flow{docker: docker, dockerProbed: true, hub: &Hub{managers: managers}}
}

func TestResolveDockerFallback_DockerRunning_SelectsFirstAdvertisedBackend(t *testing.T) {
	m := fakeManager("mind", "mind", resources.RuntimeContextFree, agentv0.Backend_LOCAL, agentv0.Backend_NIX, agentv0.Backend_DOCKER)
	flow := flowWith(DockerStatus{Running: true}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextNative {
		t.Fatalf("runtime context = %q, want %q", got, resources.RuntimeContextNative)
	}
}

func TestResolveDockerFallback_DockerRunning_SelectsContainerWhenOnlyBackend(t *testing.T) {
	m := fakeManager("infra", "postgres", resources.RuntimeContextFree, agentv0.Backend_DOCKER)
	flow := flowWith(DockerStatus{Running: true}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextContainer {
		t.Fatalf("runtime context = %q, want %q", got, resources.RuntimeContextContainer)
	}
}

func TestResolveDockerFallback_DockerDown_DockerOnlyServiceStops(t *testing.T) {
	// A container-only agent advertises DOCKER as its only backend — nothing to
	// fall back to when Docker is down.
	m := fakeManager("infra", "postgres", resources.RuntimeContextFree, agentv0.Backend_DOCKER)
	flow := flowWith(DockerStatus{Running: false, Context: "orbstack", Endpoint: "unix:///orb.sock"}, m)

	err := flow.resolveDockerFallback(context.Background())
	if err == nil {
		t.Fatal("expected a stop error when a Docker-only service can't reach Docker")
	}
	msg := err.Error()
	for _, want := range []string{"infra/postgres", "orbstack", "unix:///orb.sock"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestResolveDockerFallback_Unprobed_StillResolvesFirstBackend(t *testing.T) {
	// Test/build/deploy/sync flows do not probe Docker, but their downstream
	// boundaries still require a concrete context rather than the "free" hint.
	m := fakeManager("svc", "api", resources.RuntimeContextFree, agentv0.Backend_NIX, agentv0.Backend_DOCKER)
	flow := &Flow{hub: &Hub{managers: []IManager{m}}} // dockerProbed == false

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextNix {
		t.Fatalf("runtime context = %q, want %q", got, resources.RuntimeContextNix)
	}
}

func TestResolveDockerFallback_AgentWithoutAdvertisedBackends_LeftOnFree(t *testing.T) {
	// An agent that advertises no backends carries no capability information.
	// Rather than hard-failing the run (a regression for agents predating
	// SupportedBackends), the service is left on "free" for downstream to
	// handle — in every flow shape: docker up, docker down, and unprobed.
	for _, tc := range []struct {
		name string
		flow func(*Manager) *Flow
	}{
		{"docker running", func(m *Manager) *Flow { return flowWith(DockerStatus{Running: true}, m) }},
		{"docker down", func(m *Manager) *Flow {
			return flowWith(DockerStatus{Running: false, Context: "orbstack"}, m)
		}},
		{"unprobed", func(m *Manager) *Flow { return &Flow{hub: &Hub{managers: []IManager{m}}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := fakeManager("infra", "postgres", resources.RuntimeContextFree)
			flow := tc.flow(m)
			if err := flow.resolveDockerFallback(context.Background()); err != nil {
				t.Fatalf("unexpected error for backend-less agent: %v", err)
			}
			if got := m.Runner.runtimeContext; got != resources.RuntimeContextFree {
				t.Fatalf("runtime context = %q, want it left as %q", got, resources.RuntimeContextFree)
			}
		})
	}
}

func TestResolveDockerFallback_DockerDown_ExplicitContextHonored(t *testing.T) {
	// An explicit non-free context is never auto-resolved, even with Docker down.
	m := fakeManager("svc", "api", resources.RuntimeContextNative)
	flow := flowWith(DockerStatus{Running: false}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextNative {
		t.Fatalf("runtime context = %q, want it left as %q", got, resources.RuntimeContextNative)
	}
}

func TestResolveDockerFallback_DockerDown_NixServiceFallsBack(t *testing.T) {
	// SupportedBackends is already filtered by the agent, so the orchestrator
	// must trust an advertised Nix backend without probing the host again.
	m := fakeManager("svc", "api", resources.RuntimeContextFree, agentv0.Backend_NIX)
	flow := flowWith(DockerStatus{Running: false}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextNix {
		t.Fatalf("runtime context = %q, want %q", got, resources.RuntimeContextNix)
	}
}

func TestResolveDockerFallback_DockerDown_PrefersLocalOverNix(t *testing.T) {
	// A LOCAL-first service with no host toolchain requirement (LOCAL is always
	// available) resolves to native — honoring preference order — even when nix
	// is installed, rather than being forced onto nix.
	m := fakeManager("svc", "api", resources.RuntimeContextFree, agentv0.Backend_LOCAL, agentv0.Backend_NIX)
	flow := flowWith(DockerStatus{Running: false}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextNative {
		t.Fatalf("runtime context = %q, want %q (LOCAL preferred over NIX)", got, resources.RuntimeContextNative)
	}
}

func TestRunnerSupportsNix(t *testing.T) {
	withNix := fakeManager("svc", "api", resources.RuntimeContextFree, agentv0.Backend_NIX).Runner
	if !withNix.SupportsNix() {
		t.Fatal("expected SupportsNix() = true when NIX is advertised")
	}
	withoutNix := fakeManager("infra", "postgres", resources.RuntimeContextFree).Runner
	if withoutNix.SupportsNix() {
		t.Fatal("expected SupportsNix() = false when no runtime requirements are advertised")
	}
}

func TestDockerStatusWhere(t *testing.T) {
	cases := []struct {
		status DockerStatus
		want   string
	}{
		{DockerStatus{Context: "orbstack", Endpoint: "unix:///orb.sock"}, `docker context "orbstack" → unix:///orb.sock`},
		{DockerStatus{Endpoint: "tcp://1.2.3.4:2375"}, "endpoint tcp://1.2.3.4:2375"},
		{DockerStatus{Context: "colima"}, `docker context "colima"`},
		{DockerStatus{}, "the default docker socket"},
	}
	for _, c := range cases {
		if got := c.status.where(); got != c.want {
			t.Fatalf("where() = %q, want %q", got, c.want)
		}
	}
}
