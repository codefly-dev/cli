package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// meshWorkspace composes a host (edge/gateway: a public rest endpoint and
// the internal workload endpoint every module mints on), a module app whose
// api endpoint its own worker consumes, and a solution wiki whose backend the
// host fronts and which consumes app's api through the host.
func meshWorkspace(t *testing.T) (*resources.Workspace, *environments.Environment) {
	t.Helper()
	root := t.TempDir()
	service := func(name, endpoints string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n" + endpoints
	}
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: example
layout: modules
modules:
  - name: edge
  - name: app
  - name: wiki
environments:
  - name: prod
    namespace: example
    host:
      coordinate: example/local/dev
      component: host
      domain: example
      audience: mint
      trust_domain: local.example
      envelope_revision: 1
      delivery: edge/gateway/rest
      gateway: edge/gateway
`,
		"modules/edge/" + resources.ModuleConfigurationName: "kind: module\nname: edge\nservices:\n  - name: gateway\n",
		"modules/edge/services/gateway/" + resources.ServiceConfigurationName: service("gateway",
			"endpoints:\n  - name: rest\n    api: rest\n    visibility: public\n    exposure: public\n  - name: workload\n    api: http\n    visibility: internal\n"),
		"modules/app/" + resources.ModuleConfigurationName: "kind: module\nname: app\nservices:\n  - name: api\n  - name: worker\n",
		"modules/app/services/api/" + resources.ServiceConfigurationName: service("api",
			"endpoints:\n  - name: grpc\n    api: grpc\n    visibility: internal\n  - name: admin\n    api: rest\n"),
		"modules/app/services/worker/" + resources.ServiceConfigurationName: service("worker",
			"service-dependencies:\n  - name: api\n    endpoints:\n      - name: grpc\n"),
		"modules/wiki/" + resources.ModuleConfigurationName: "kind: module\nname: wiki\nservice-entry: backend\nservices:\n  - name: backend\n",
		"modules/wiki/services/backend/" + resources.ServiceConfigurationName: service("backend",
			"endpoints:\n  - name: http\n    api: http\n    visibility: internal\n"),
		"modules/wiki/services/backend/" + solutionrun.FrontendFileName: "title: Wiki\norder: 1\nhost_contract: 1\n",
		"modules/wiki/solution.codefly.yaml": `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: wiki
  version: 0.1.0
api:
  exposes:
    - id: http
      protocol: http
  consumes:
    - id: app
      protocol: grpc
      module: app
      service: api
      endpoint: grpc
      as: app
lifecycle:
  render: true
`,
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	env, err := environments.Select(workspace, "prod")
	require.NoError(t, err)
	return workspace, env
}

func moduleServices(t *testing.T, workspace *resources.Workspace, name string) (*resources.Module, []*resources.Service) {
	t.Helper()
	module, err := workspace.LoadModuleFromName(t.Context(), name)
	require.NoError(t, err)
	services, err := loadModuleServices(t.Context(), module)
	require.NoError(t, err)
	return module, services
}

// The edges a tree carries are the ones INTO its module's workloads, each
// sourced from the composition's own declarations: a module endpoint admits
// its declared consumers and, when a solution consumes it through the host,
// the gateway; a solution's backend admits the gateway; the gateway's
// workload port admits every composed workload and its public port the
// cell's ingress. An endpoint nobody reaches has no edge.
func TestMeshEdgesAreTheCompositionsDeclaredReach(t *testing.T) {
	workspace, env := meshWorkspace(t)
	const domain = "spiffe://local.example/ns/example-"
	gateway, api, worker, backend := domain+"edge/sa/gateway", domain+"app/sa/api", domain+"app/sa/worker", domain+"wiki/sa/backend"
	ports := map[string]map[string]uint32{
		"edge/gateway": {"rest": 8080, "workload": 8081},
		"app/api":      {"grpc": 9090, "admin": 8082},
		"wiki/backend": {"http": 8080},
	}

	t.Run("a module endpoint admits its consumers and the gateway", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "app")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports)
		require.NoError(t, err)
		require.Equal(t, MeshEdgesSchemaVersion, edges.SchemaVersion)
		require.Equal(t, "local.example", edges.TrustDomain)
		require.Equal(t, []MeshEdge{{
			Module: "app", Service: "api", Namespace: "example-app", Account: "api", Principal: api,
			Endpoint: "grpc", Port: 9090,
			// Every workload of the consumer module, and the gateway the
			// solution consumes through; the private admin endpoint has no
			// edge at all.
			Principals: []string{api, worker, gateway},
		}}, edges.Edges)
	})
	t.Run("a solution's backend admits the gateway", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "wiki")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports)
		require.NoError(t, err)
		require.Equal(t, []MeshEdge{{
			Module: "wiki", Service: "backend", Namespace: "example-wiki", Account: "backend", Principal: backend,
			Endpoint: "http", Port: 8080, Principals: []string{gateway},
		}}, edges.Edges)
	})
	t.Run("the gateway admits every workload on its workload port and the ingress on its public one", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "edge")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports)
		require.NoError(t, err)
		require.Equal(t, []MeshEdge{
			{Module: "edge", Service: "gateway", Namespace: "example-edge", Account: "gateway", Principal: gateway, Endpoint: "rest", Port: 8080, Ingress: true},
			{Module: "edge", Service: "gateway", Namespace: "example-edge", Account: "gateway", Principal: gateway, Endpoint: "workload", Port: 8081, Principals: []string{api, worker, gateway, backend}},
		}, edges.Edges)
	})
	t.Run("no host, no mesh, no edges", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "app")
		hostless := *env
		hostless.Host = nil
		edges, err := deriveMeshEdges(t.Context(), workspace, &hostless, module, services, ports)
		require.NoError(t, err)
		require.Nil(t, edges)
	})
	t.Run("an endpoint with no in-cluster port is not in the cell", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "wiki")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, map[string]map[string]uint32{})
		require.NoError(t, err)
		require.Empty(t, edges.Edges)
	})
}
