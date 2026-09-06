package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestServerCommandHasOpenFlag(t *testing.T) {
	if ServerCmd.Flags().Lookup("open") == nil {
		t.Fatal("codefly server has no --open flag")
	}
}

type fakeServerCLI struct {
	cliv0.UnimplementedCLIServer
	name string
}

func (f *fakeServerCLI) GetWorkspaceInventory(context.Context, *emptypb.Empty) (*basev0.Workspace, error) {
	return &basev0.Workspace{Name: f.name}, nil
}

// TestServerCommandAttachesInsteadOfStartingASecondServer exercises the
// actual behavior change this issue introduces: `codefly server` used to
// unconditionally start a new server, which would fail with "address already
// in use" against a workspace that already has one running. It must instead
// detect the running dashboard and attach (print its URL, exit 0) rather
// than double-binding.
func TestServerCommandAttachesInsteadOfStartingASecondServer(t *testing.T) {
	const workspaceName = "server-attach-test"
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "workspace.codefly.yaml"),
		[]byte("name: "+workspaceName+"\nlayout: flat\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	addr := fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(workspaceName))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	grpcServer := grpc.NewServer()
	cliv0.RegisterCLIServer(grpcServer, &fakeServerCLI{name: workspaceName})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)

	var messages []string
	cli.SetOutputSink(func(_ wool.Loglevel, msg string) { messages = append(messages, msg) })
	t.Cleanup(func() { cli.SetOutputSink(nil) })

	if err := ServerCmd.RunE(ServerCmd, nil); err != nil {
		t.Fatalf("ServerCmd.RunE() = %v, want nil (should attach, not fail)", err)
	}

	for _, msg := range messages {
		if strings.Contains(msg, "Dashboard already served") && strings.Contains(msg, workspaceName) {
			return
		}
	}
	t.Fatalf("expected an 'already served' message naming %q, got: %v", workspaceName, messages)
}
