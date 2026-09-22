package go_grpc

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestConfigurationRPCIncludesTheActiveEnvironmentScope(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, filepath.Join("..", "..", "orchestration", "testdata", "module-layout"))
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(ctx, workspace, nil, nil, &environments.Environment{Name: "staging"}, orchestration.RunMode)
	require.NoError(t, err)
	flows := engine.NewFlowManager()
	require.NoError(t, flows.Register("configuration", flow))
	server, err := NewServer(&Configuration{EndpointGrpc: "127.0.0.1:0"}, workspace, flows)
	require.NoError(t, err)
	listener, err := server.Listen()
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() { cancel(); require.NoError(t, <-done) })
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	response, err := cli.NewCLIClient(connection).GetConfiguration(ctx, &cli.GetConfigurationRequest{Module: "management", Service: "organization"})
	require.NoError(t, err)
	require.Len(t, response.ProcessVariables, 1)
	require.Equal(t, resources.EnvironmentPrefix, response.ProcessVariables[0].Key)
	require.Equal(t, "staging", response.ProcessVariables[0].Value)
}
