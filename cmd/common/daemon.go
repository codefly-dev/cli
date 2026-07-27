package common

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	cliv0 "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/network"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// DialDaemon opens a gRPC client to the workspace's CLI server (the daemon
// started by `codefly run`). The server listens on a deterministic port
// derived from the workspace name, so no discovery file is needed.
//
// The returned client is nil if a connection could not be created — callers
// then fall back to the static resource model. grpc.NewClient is lazy, so a
// nil error here does NOT guarantee the daemon is up; RPCs surface that.
func DialDaemon(workspaceName, namingScope string) (cliv0.CLIClient, func()) {
	addr := fmt.Sprintf("127.0.0.1:%d", network.CLIServerPort(scopedWorkspaceName(workspaceName, namingScope)))
	probe, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
	if err != nil {
		return nil, nil
	}
	_ = probe.Close()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil
	}
	return cliv0.NewCLIClient(conn), func() { _ = conn.Close() }
}

// ResolveAddress asks the daemon for the concrete address (host:port) of one
// endpoint. Bounded by a short timeout so a dead daemon fails fast.
func ResolveAddress(ctx context.Context, client cliv0.CLIClient, module, service, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := client.GetAddresses(ctx, &cliv0.GetAddressRequest{
		Module:   module,
		Service:  service,
		Endpoint: endpoint,
	})
	if err != nil {
		return "", err
	}
	return resp.Address, nil
}

func scopedWorkspaceName(workspaceName, namingScope string) string {
	workspaceName = strings.TrimSpace(workspaceName)
	namingScope = strings.TrimSpace(namingScope)
	if namingScope == "" {
		return workspaceName
	}
	return workspaceName + "-" + namingScope
}
