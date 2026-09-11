package web

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/sdk/session"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestIsolatedSessionServesSDKControlSocket(t *testing.T) {
	workspace := &resources.Workspace{Name: "isolated-session-test"}
	shared := newTestServer(t, workspace)
	require.NoError(t, shared.Listen())
	t.Cleanup(shared.Close)

	var targets []*session.Control
	var owners []*session.Session
	var clients []cli.CLIClient
	for range 2 {
		owner, err := session.New()
		require.NoError(t, err)
		control, err := session.NewControl()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, control.Remove()) })
		t.Setenv(session.IDEnvironment, owner.ID)
		t.Setenv(session.SecretEnvironment, owner.Secret)
		t.Setenv(session.SocketEnvironment, control.Socket)
		server := newTestServer(t, workspace)
		require.Empty(t, server.DashboardURL())
		require.Nil(t, server.rest)
		require.NoError(t, server.Listen())
		duplicate := newTestServer(t, workspace)
		require.Error(t, duplicate.Listen())
		duplicate.Close()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- server.Start(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Error("isolated server did not stop")
			}
			_, err := os.Lstat(control.Socket)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
		conn, err := grpc.NewClient(control.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, conn.Close()) })
		client := cli.NewCLIClient(conn)
		callCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		_, err = client.Ping(callCtx, &emptypb.Empty{})
		stop()
		require.NoError(t, err)
		targets = append(targets, control)
		owners = append(owners, owner)
		clients = append(clients, client)
	}
	require.NotEqual(t, targets[0].Socket, targets[1].Socket)
	for i, client := range clients {
		challenge, err := session.Challenge()
		require.NoError(t, err)
		req := &cli.SessionHandshakeRequest{SessionId: owners[i].ID, Challenge: challenge, ProtocolVersion: session.ProtocolVersion}
		response, err := client.SessionHandshake(t.Context(), req)
		require.NoError(t, err)
		require.Equal(t, owners[i].ID, response.SessionId)
		require.Equal(t, session.ProtocolVersion, int(response.ProtocolVersion))
		require.True(t, owners[i].VerifyProof(challenge, response.Proof))
		require.False(t, owners[1-i].VerifyProof(challenge, response.Proof))
		require.Contains(t, response.Capabilities, session.IsolatedControlSocketCapability)
		req.SessionId = owners[1-i].ID
		_, err = client.SessionHandshake(t.Context(), req)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		req.SessionId = owners[i].ID
		req.ProtocolVersion++
		_, err = client.SessionHandshake(t.Context(), req)
		require.Equal(t, codes.FailedPrecondition, status.Code(err))
		req.ProtocolVersion = session.ProtocolVersion
		req.Challenge = ""
		_, err = client.SessionHandshake(t.Context(), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
}

func TestSessionEnvironmentCannotFallBackToTCP(t *testing.T) {
	owner, err := session.New()
	require.NoError(t, err)
	control, err := session.NewControl()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, control.Remove()) })
	for _, values := range [][3]string{
		{"", "", control.Socket}, {owner.ID, "", control.Socket},
		{owner.ID, owner.Secret, ""}, {owner.ID, owner.Secret, "relative.sock"},
	} {
		t.Setenv(session.IDEnvironment, values[0])
		t.Setenv(session.SecretEnvironment, values[1])
		t.Setenv(session.SocketEnvironment, values[2])
		_, err := NewServer(ServerData{})
		require.Error(t, err)
	}
	t.Setenv(session.IDEnvironment, owner.ID)
	t.Setenv(session.SecretEnvironment, owner.Secret)
	t.Setenv(session.SocketEnvironment, control.Socket)
	require.NoError(t, os.Chmod(filepath.Dir(control.Socket), 0o755))
	_, err = NewServer(ServerData{})
	require.ErrorContains(t, err, "private")
}

func TestIsolatedSessionDoesNotUnlinkForeignSocket(t *testing.T) {
	owner, err := session.New()
	require.NoError(t, err)
	control, err := session.NewControl()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, control.Remove()) })
	listener, err := net.Listen("unix", control.Socket)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	t.Setenv(session.IDEnvironment, owner.ID)
	t.Setenv(session.SecretEnvironment, owner.Secret)
	t.Setenv(session.SocketEnvironment, control.Socket)
	server := newTestServer(t, nil)
	require.ErrorContains(t, server.Listen(), "refusing to displace an existing owner")
	server.Close()
	conn, err := net.DialTimeout("unix", control.Socket, time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// The pinned Core SDK requires this handshake by default. Exercise the outer
// server used by run service, including its listener ownership and shutdown.
func TestServerSupportsCoreIsolatedSession(t *testing.T) {
	owner, err := session.New()
	require.NoError(t, err)
	control, err := session.NewControl()
	require.NoError(t, err)
	defer control.Remove()
	t.Setenv(session.IDEnvironment, owner.ID)
	t.Setenv(session.SecretEnvironment, owner.Secret)
	t.Setenv(session.SocketEnvironment, control.Socket)
	t.Setenv(session.PortEnvironment, "")
	server, err := NewServer(ServerData{NamingScope: owner.Scope()})
	require.NoError(t, err)
	require.NoError(t, server.Listen())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- server.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-result:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("server did not release the isolated session")
		}
	}()
	conn, err := grpc.NewClient(control.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	challenge, err := session.Challenge()
	require.NoError(t, err)
	response, err := cli.NewCLIClient(conn).SessionHandshake(ctx, &cli.SessionHandshakeRequest{
		SessionId: owner.ID, Challenge: challenge, ProtocolVersion: session.ProtocolVersion,
	})
	require.NoError(t, err)
	require.True(t, owner.VerifyProof(challenge, response.Proof))
	require.Contains(t, response.Capabilities, session.IsolatedControlSocketCapability)
}
