package web

import (
	"context"
	"testing"
	"time"

	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/sdk/session"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

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
