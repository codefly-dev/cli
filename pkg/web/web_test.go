package web

import (
	"testing"

	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
)

func TestLoopbackEndpointDoesNotExposeCLIServer(t *testing.T) {
	if got, want := loopbackEndpoint(uint16(10000)), "127.0.0.1:10000"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestDashboardURLMatchesDerivedRESTEndpoint guards CodeflyServer.DashboardURL,
// which #536 depends on: a typo in its "http://" + address composition would
// otherwise ship silently since nothing previously called or tested it.
func TestDashboardURLMatchesDerivedRESTEndpoint(t *testing.T) {
	server, err := NewServer(ServerData{Workspace: &resources.Workspace{Name: "dashboard-url-test"}})
	if err != nil {
		t.Fatal(err)
	}

	want := "http://" + loopbackEndpoint(network.CLIRestPort("dashboard-url-test"))
	if got := server.DashboardURL(); got != want {
		t.Fatalf("DashboardURL() = %q, want %q", got, want)
	}
}
