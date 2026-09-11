package go_grpc

import (
	"context"
	"testing"
	"time"

	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/sdk/session"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestSessionHandshakeUsesOwnedUnixSocketAndRejectsForeignSessions(t *testing.T) {
	owner, err := session.New()
	require.NoError(t, err)
	control, err := session.NewControl()
	require.NoError(t, err)
	defer control.Remove()
	t.Setenv(session.IDEnvironment, owner.ID)
	t.Setenv(session.SecretEnvironment, owner.Secret)
	t.Setenv(session.SocketEnvironment, control.Socket)
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0", ControlSocket: control.Socket, Session: owner}, nil, nil)
	require.NoError(t, err)
	listener, err := server.Listen()
	require.NoError(t, err)
	require.Equal(t, "unix", listener.Addr().Network())
	go server.gRPC.Serve(listener)
	defer server.gRPC.Stop()
	defer server.Close()
	conn, err := grpc.NewClient(control.Target(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := cli.NewCLIClient(conn)
	challenge, err := session.Challenge()
	require.NoError(t, err)
	request := &cli.SessionHandshakeRequest{SessionId: owner.ID, Challenge: challenge, ProtocolVersion: session.ProtocolVersion}
	response, err := client.SessionHandshake(ctx, request)
	require.NoError(t, err)
	require.True(t, owner.VerifyProof(challenge, response.Proof))
	require.Contains(t, response.Capabilities, session.IsolatedControlSocketCapability)
	request.SessionId = "foreign"
	_, err = client.SessionHandshake(ctx, request)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	request.SessionId = owner.ID
	request.ProtocolVersion++
	_, err = client.SessionHandshake(ctx, request)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	request.ProtocolVersion = session.ProtocolVersion
	request.Challenge = ""
	_, err = client.SessionHandshake(ctx, request)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	request.Challenge = challenge
	_, err = server.SessionHandshake(context.Background(), request)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	t.Setenv(session.SocketEnvironment, "")
	public, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0"}, nil, nil)
	require.NoError(t, err)
	tcp, err := public.Listen()
	require.NoError(t, err)
	go public.gRPC.Serve(tcp)
	defer public.gRPC.Stop()
	defer public.Close()
	publicConn, err := grpc.NewClient(tcp.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer publicConn.Close()
	_, err = cli.NewCLIClient(publicConn).SessionHandshake(ctx, request)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

}
