package go_grpc

import (
	"context"
	"os"
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
	flow.WithRuntimeContext(resources.RuntimeContextNative)
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

// The dependency SDK consumes ProcessVariables verbatim. Workspace groups must
// reach that carrier, including declared credentials, while credentials from an
// undeclared composition-root group remain withheld.
func TestConfigurationRPCDeliversResolvedWorkspaceGroups(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	for name, contents := range map[string]string{
		"workspace.codefly.yaml":                                "name: configuration-rpc\nlayout: modules\nmodules:\n  - name: consumer\n",
		"modules/consumer/module.codefly.yaml":                  "kind: module\nname: consumer\nservices:\n  - name: caller\n",
		"modules/consumer/services/caller/service.codefly.yaml": "kind: service\nname: caller\nversion: 0.0.0\nmodule: consumer\nworkspace-configuration-dependencies: [declared]\n",
		"configurations/local/declared.env":                     "setting=visible\n",
		"configurations/local/declared.secret.env":              "credential=declared-test-value\n",
		"configurations/local/root.env":                         "setting=inherited\n",
		"configurations/local/root.secret.env":                  "credential=withheld-test-value\n",
	} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	env := &environments.Environment{Name: "local"}
	flow, err := orchestration.NewFlow(ctx, workspace, nil, nil, env, orchestration.RunMode)
	require.NoError(t, err)
	require.NoError(t, flow.ConfigurationManager.Load(ctx, env.Runtime()))
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flows := engine.NewFlowManager()
	require.NoError(t, flows.Register("configuration", flow))
	server, err := NewServer(&Configuration{}, workspace, flows)
	require.NoError(t, err)
	response, err := server.GetConfiguration(ctx, &cli.GetConfigurationRequest{Module: "consumer", Service: "caller"})
	require.NoError(t, err)
	values := map[string]string{}
	for _, variable := range response.ProcessVariables {
		values[variable.Key] = variable.Value
	}
	require.Equal(t, "local", values[resources.EnvironmentPrefix])
	require.Equal(t, "visible", values["CODEFLY__WORKSPACE_CONFIGURATION__DECLARED__SETTING"])
	require.Equal(t, "declared-test-value", values["CODEFLY__WORKSPACE_SECRET_CONFIGURATION__DECLARED__CREDENTIAL"])
	require.Equal(t, "inherited", values["CODEFLY__WORKSPACE_CONFIGURATION__ROOT__SETTING"])
	require.NotContains(t, values, "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__ROOT__CREDENTIAL")
}
