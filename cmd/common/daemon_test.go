package common

import (
	"context"
	"fmt"
	"net"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/network"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

type fakeCLIServer struct {
	cliv0.UnimplementedCLIServer
	name string
}

func (f *fakeCLIServer) GetWorkspaceInventory(context.Context, *emptypb.Empty) (*basev0.Workspace, error) {
	return &basev0.Workspace{Name: f.name}, nil
}

func startFakeCLIServer(t *testing.T, listenName, reportedName string) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(listenName))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	s := grpc.NewServer()
	cliv0.RegisterCLIServer(s, &fakeCLIServer{name: reportedName})
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
}

func TestDashboardAttachedConfirmsIdentityOverGRPC(t *testing.T) {
	startFakeCLIServer(t, "dashboard-attached-match", "dashboard-attached-match")
	if !DashboardAttached(context.Background(), "dashboard-attached-match") {
		t.Fatal("DashboardAttached() = false for a real CLI server reporting the matching workspace name")
	}
}

func TestDashboardAttachedRejectsNameMismatch(t *testing.T) {
	startFakeCLIServer(t, "dashboard-attached-mismatch", "some-other-workspace")
	if DashboardAttached(context.Background(), "dashboard-attached-mismatch") {
		t.Fatal("DashboardAttached() = true for a CLI server reporting a different workspace name")
	}
}

// TestDashboardAttachedRejectsNonCLIListener is the regression test for the
// false-positive attach bug: a bare TCP listener on the hashed port (any
// unrelated process — a database, another CLI, anything) must NOT be mistaken
// for a running codefly dashboard just because something answers the socket.
func TestDashboardAttachedRejectsNonCLIListener(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort("dashboard-attached-unrelated"))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	defer lis.Close()

	if DashboardAttached(context.Background(), "dashboard-attached-unrelated") {
		t.Fatal("DashboardAttached() = true for a plain TCP listener that isn't a codefly CLI server")
	}
}

func TestDashboardAttachedFalseWhenNothingListening(t *testing.T) {
	if DashboardAttached(context.Background(), "dashboard-attached-nothing-here") {
		t.Fatal("DashboardAttached() = true with no listener at all")
	}
}

func TestScopedWorkspaceName(t *testing.T) {
	tests := []struct {
		workspace string
		scope     string
		want      string
	}{
		{workspace: "warden-platform", want: "warden-platform"},
		{workspace: "warden-platform", scope: "dev", want: "warden-platform-dev"},
		{workspace: " warden-platform ", scope: " stable ", want: "warden-platform-stable"},
	}
	for _, test := range tests {
		if got := scopedWorkspaceName(test.workspace, test.scope); got != test.want {
			t.Errorf("scopedWorkspaceName(%q, %q) = %q, want %q", test.workspace, test.scope, got, test.want)
		}
	}
}
