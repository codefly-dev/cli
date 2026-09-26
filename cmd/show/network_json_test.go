package show

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeNetworkWorkspace lays down a workspace composing one module under the
// given name, whose "accounts" service exposes a named "authority" gRPC
// endpoint beside its conventional grpc, connect and rest endpoints.
func writeNetworkWorkspace(t *testing.T, module string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: "name: acme\nlayout: modules\nmodules:\n  - name: " + module + "\n" +
			"environments:\n  - name: staging\n    namespace: acme\n",
		filepath.Join("modules", module, resources.ModuleConfigurationName): "kind: module\nname: " + module + "\nservices:\n  - name: accounts\n",
		filepath.Join("modules", module, "services", "accounts", resources.ServiceConfigurationName): `kind: service
name: accounts
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
endpoints:
  - name: authority
    api: grpc
  - name: connect
    api: connect
  - name: grpc
    api: grpc
  - name: rest
    api: rest
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	return root
}

func runNetworkJSON(t *testing.T, root, env string) networkReport {
	t.Helper()
	t.Chdir(root)
	previousJSON, previousEnv := showNetworkJSON, showNetworkEnv
	t.Cleanup(func() {
		showNetworkJSON, showNetworkEnv = previousJSON, previousEnv
		NetworkCmd.SetOut(nil)
	})
	showNetworkJSON, showNetworkEnv = true, env
	var out bytes.Buffer
	NetworkCmd.SetOut(&out)
	require.NoError(t, NetworkCmd.RunE(NetworkCmd, nil))
	var report networkReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report), out.String())
	return report
}

func deployedPorts(t *testing.T, report networkReport) map[string]uint16 {
	t.Helper()
	require.Len(t, report.Services, 1)
	ports := map[string]uint16{}
	for _, endpoint := range report.Services[0].Endpoints {
		require.NotNil(t, endpoint.DeployedPort, endpoint.Name)
		require.NotEmpty(t, endpoint.Native, endpoint.Name)
		ports[endpoint.Name] = *endpoint.DeployedPort
	}
	return ports
}

// The JSON read path reports the same in-cluster ports the render allocates,
// hashed on the name the workspace composes the module under.
func TestShowNetworkJSONReportsDeployedPorts(t *testing.T) {
	report := runNetworkJSON(t, writeNetworkWorkspace(t, "saas"), "staging")
	require.Equal(t, "acme", report.Workspace)
	require.Equal(t, "staging", report.Environment)
	require.Equal(t, "saas/accounts", report.Services[0].Service)
	require.Equal(t, map[string]uint16{"authority": 52893, "connect": 8081, "grpc": 9090, "rest": 8080}, deployedPorts(t, report))

	report = runNetworkJSON(t, writeNetworkWorkspace(t, "saas-starter"), "staging")
	require.Equal(t, uint16(6003), deployedPorts(t, report)["authority"])
}

func TestShowNetworkJSONRefusesAnUndeclaredEnvironment(t *testing.T) {
	root := writeNetworkWorkspace(t, "saas")
	t.Chdir(root)
	previousJSON, previousEnv := showNetworkJSON, showNetworkEnv
	t.Cleanup(func() { showNetworkJSON, showNetworkEnv = previousJSON, previousEnv })
	showNetworkJSON, showNetworkEnv = true, "production"
	require.ErrorContains(t, NetworkCmd.RunE(NetworkCmd, nil), `does not declare environment "production"`)
}
