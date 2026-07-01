package orchestration

import (
	"context"
	"strings"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/services"
)

func fakeManager(module, name, runtimeContext string, runtimes ...agentv0.Runtime_Type) *Manager {
	reqs := make([]*agentv0.Runtime, 0, len(runtimes))
	for _, t := range runtimes {
		reqs = append(reqs, &agentv0.Runtime{Type: t})
	}
	instance := &services.Instance{
		Identity: &resources.ServiceIdentity{Module: module, Name: name},
		Info:     &agentv0.AgentInformation{RuntimeRequirements: reqs},
	}
	return &Manager{Runner: &Runner{instance: instance, runtimeContext: runtimeContext}}
}

func flowWith(docker DockerStatus, managers ...IManager) *Flow {
	return &Flow{docker: docker, hub: &Hub{managers: managers}}
}

func TestResolveDockerFallback_DockerRunning_LeavesContextsUntouched(t *testing.T) {
	m := fakeManager("infra", "postgres", resources.RuntimeContextFree)
	flow := flowWith(DockerStatus{Running: true}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextFree {
		t.Fatalf("runtime context = %q, want it left as %q", got, resources.RuntimeContextFree)
	}
}

func TestResolveDockerFallback_DockerDown_DockerOnlyServiceStops(t *testing.T) {
	// A container-only agent advertises no nix/native runtime requirements.
	m := fakeManager("infra", "postgres", resources.RuntimeContextFree)
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
	if !(runnersbase.CheckNixInstalled() && runnersbase.IsNixSupported()) {
		t.Skip("nix not available on this machine")
	}
	// A nix-capable service falls back to nix rather than stopping the run.
	m := fakeManager("svc", "api", resources.RuntimeContextFree, agentv0.Runtime_GO, agentv0.Runtime_NIX)
	flow := flowWith(DockerStatus{Running: false}, m)

	if err := flow.resolveDockerFallback(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := m.Runner.runtimeContext; got != resources.RuntimeContextNix {
		t.Fatalf("runtime context = %q, want %q", got, resources.RuntimeContextNix)
	}
}

func TestRunnerSupportsNix(t *testing.T) {
	withNix := fakeManager("svc", "api", resources.RuntimeContextFree, agentv0.Runtime_GO, agentv0.Runtime_NIX).Runner
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
