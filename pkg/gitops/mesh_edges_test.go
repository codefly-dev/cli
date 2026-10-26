package gitops

import (
	"os"
	"path/filepath"
	"strings"
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
  - name: exec
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
      delivery: edge/gateway/rest
      gateway: edge/gateway
    ingress:
      - name: portal
        service: edge/gateway
        endpoint: rest
        hosts: [app.example.test]
`,
		"modules/edge/" + resources.ModuleConfigurationName: "kind: module\nname: edge\nservices:\n  - name: gateway\n",
		"modules/edge/services/gateway/" + resources.ServiceConfigurationName: service("gateway",
			"endpoints:\n  - name: rest\n    api: rest\n    visibility: public\n    exposure: public\n  - name: workload\n    api: http\n    visibility: internal\n"),
		"modules/app/" + resources.ModuleConfigurationName: "kind: module\nname: app\nservices:\n  - name: api\n  - name: worker\n",
		"modules/app/services/api/" + resources.ServiceConfigurationName: service("api",
			"endpoints:\n  - name: grpc\n    api: grpc\n    visibility: internal\n  - name: connect\n    api: connect\n    visibility: internal\n  - name: admin\n    api: rest\n"),
		// app derives one operation on api's grpc publication: a prepared binding
		// calls it on api's connect endpoint, and whoever installs the bindings
		// reaches that endpoint.
		"modules/app/contracts/runnables/index.json":        `{"schema":"codefly/runnable-operations/v1","workspace":"example","module":"app","operations":[{"name":"do","version":"0.0.1","digest":"sha256:0","service":"api","endpoint":"grpc","method":"Do","input_message":"DoRequest","output_message":"DoResponse","path":"do"}]}`,
		"modules/exec/" + resources.ModuleConfigurationName: "kind: module\nname: exec\nservices:\n  - name: runner\n",
		"modules/exec/services/runner/" + resources.ServiceConfigurationName: service("runner",
			"workspace-configuration-dependencies:\n  - runnable-bindings\n"),
		"modules/app/services/worker/" + resources.ServiceConfigurationName: service("worker",
			"service-dependencies:\n  - name: api\n    endpoints:\n      - name: grpc\n"),
		"modules/wiki/" + resources.ModuleConfigurationName: "kind: module\nname: wiki\nservice-entry: backend\nservices:\n  - name: backend\n",
		"modules/wiki/services/backend/" + resources.ServiceConfigurationName: service("backend",
			"endpoints:\n  - name: http\n    api: http\n    visibility: internal\n"),
		"modules/wiki/services/backend/" + solutionrun.FrontendFileName: "title: Wiki\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n",
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
	services, err := loadModuleServices(t.Context(), module, nil)
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
	gateway, api, worker, backend, runner := domain+"edge/sa/gateway", domain+"app/sa/api", domain+"app/sa/worker", domain+"wiki/sa/backend", domain+"exec/sa/runner"
	ports := map[string]map[string]uint32{
		"edge/gateway": {"rest": 8080, "workload": 8081},
		"app/api":      {"grpc": 9090, "connect": 8081, "admin": 8082},
		"wiki/backend": {"http": 8080},
	}

	t.Run("a module endpoint admits its consumers and the gateway", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "app")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports, nil)
		require.NoError(t, err)
		require.Equal(t, MeshEdgesSchemaVersion, edges.SchemaVersion)
		require.Equal(t, "local.example", edges.TrustDomain)
		require.Equal(t, []MeshEdge{{
			// The executor installing the prepared bindings reaches the connect
			// endpoint the derived operation is called on, and nobody else:
			// the composition prepared that call, it declared no dependency.
			Module: "app", Service: "api", Namespace: "example-app", Account: "api", Principal: api,
			Endpoint: "connect", Port: 8081, TargetPort: 8081, Principals: []string{runner},
		}, {
			Module: "app", Service: "api", Namespace: "example-app", Account: "api", Principal: api,
			Endpoint: "grpc", Port: 9090, TargetPort: 9090,
			// Every workload of the consumer module, and the gateway the
			// solution consumes through; the private admin endpoint has no
			// edge at all.
			Principals: []string{api, worker, gateway},
		}}, edges.Edges)
	})
	t.Run("a solution's backend admits the gateway", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "wiki")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports, nil)
		require.NoError(t, err)
		require.Equal(t, []MeshEdge{{
			Module: "wiki", Service: "backend", Namespace: "example-wiki", Account: "backend", Principal: backend,
			Endpoint: "http", Port: 8080, TargetPort: 8080, Principals: []string{gateway},
		}}, edges.Edges)
	})
	t.Run("the gateway admits every workload on its workload port and the ingress on its public one", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "edge")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports, nil)
		require.NoError(t, err)
		require.Equal(t, "edge/gateway", edges.Gateway)
		require.Equal(t, []MeshEdge{
			{Module: "edge", Service: "gateway", Namespace: "example-edge", Account: "gateway", Principal: gateway, Endpoint: "rest", Port: 8080, TargetPort: 8080, Ingress: true},
			{Module: "edge", Service: "gateway", Namespace: "example-edge", Account: "gateway", Principal: gateway, Endpoint: "workload", Port: 8081, TargetPort: 8081, Principals: []string{api, worker, gateway, runner, backend}},
		}, edges.Edges)
	})
	t.Run("service and container ports are independently carried", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "edge")
		targets := map[string]map[string]uint32{"edge/gateway": {"rest": 3000, "workload": 3001}}
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, targets, nil)
		require.NoError(t, err)
		require.EqualValues(t, 8080, edges.Edges[0].Port)
		require.EqualValues(t, 3000, edges.Edges[0].TargetPort)
		delete(targets["edge/gateway"], "workload")
		_, err = deriveMeshEdges(t.Context(), workspace, env, module, services, ports, targets, nil)
		require.ErrorContains(t, err, "no resolved container target port")
	})
	t.Run("no host, no mesh, no edges", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "app")
		hostless := *env
		hostless.Host = nil
		edges, err := deriveMeshEdges(t.Context(), workspace, &hostless, module, services, ports, ports, nil)
		require.NoError(t, err)
		require.Nil(t, edges)
	})
	t.Run("an endpoint with no in-cluster port is not in the cell", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "wiki")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, map[string]map[string]uint32{}, nil, nil)
		require.NoError(t, err)
		require.Empty(t, edges.Edges)
	})
}

// The cell's ingress reaches exactly what the environment routes: a public
// endpoint nobody routes has no ingress edge, a route onto an endpoint its
// module does not declare public is refused by name, and a routed public
// endpoint of any module — a product's portal, not only the host gateway —
// is an ingress edge.
func TestIngressEdgesAreTheEnvironmentsRoutes(t *testing.T) {
	workspace, env := meshWorkspace(t)
	// api's connect endpoint is left without an in-cluster port here: this test
	// is about the self-consumed grpc edge alone.
	ports := map[string]map[string]uint32{"edge/gateway": {"rest": 8080, "workload": 8081}, "app/api": {"grpc": 9090, "admin": 8082}}
	module, services := moduleServices(t, workspace, "edge")
	routed := *env
	routed.Ingress = nil
	edges, err := deriveMeshEdges(t.Context(), workspace, &routed, module, services, ports, ports, nil)
	require.NoError(t, err)
	for _, edge := range edges.Edges {
		require.False(t, edge.Ingress, "a public endpoint the environment does not route has no ingress edge")
	}
	onInternal := *env
	onInternal.Ingress = []environments.EnvironmentIngressRoute{{Name: "admin", Service: "app/api", Endpoint: "admin", Hosts: []string{"admin.example.test"}}}
	appModule, appServices := moduleServices(t, workspace, "app")
	_, err = deriveMeshEdges(t.Context(), workspace, &onInternal, appModule, appServices, ports, ports, nil)
	require.ErrorContains(t, err, "routes app/api/admin at its edge")
	require.ErrorContains(t, err, "not public")
}

// A service whose render includes a workload that does not serve its endpoints
// — an agent's bootstrap Job, running under the service's own ServiceAccount —
// reaches those endpoints as a client of itself. No composition declares that:
// it is one rendered workload of a service reaching another, so the render is
// where it enters. Without it the cell's default-deny mesh refuses a Postgres
// bootstrap Job the database it exists to initialize.
func TestAServiceRenderingANonServingWorkloadReachesItself(t *testing.T) {
	workspace, env := meshWorkspace(t)
	const domain = "spiffe://local.example/ns/example-"
	gateway, api, worker, backend := domain+"edge/sa/gateway", domain+"app/sa/api", domain+"app/sa/worker", domain+"wiki/sa/backend"
	ports := map[string]map[string]uint32{
		"edge/gateway": {"rest": 8080, "workload": 8081},
		"app/api":      {"grpc": 9090, "admin": 8082},
		"wiki/backend": {"http": 8080},
	}

	t.Run("its own principal joins the endpoint's", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "wiki")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports,
			map[string]bool{"wiki/backend": true})
		require.NoError(t, err)
		require.Equal(t, []MeshEdge{{
			Module: "wiki", Service: "backend", Namespace: "example-wiki", Account: "backend", Principal: backend,
			Endpoint: "http", Port: 8080, TargetPort: 8080, Principals: []string{gateway, backend},
		}}, edges.Edges)
	})
	t.Run("an endpoint only it reaches gets the edge it would otherwise not have", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "app")
		// Nobody is declared to reach api's private admin endpoint, so without
		// a non-serving workload it has no edge at all.
		plain, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports, nil)
		require.NoError(t, err)
		require.Len(t, plain.Edges, 1)

		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports,
			map[string]bool{"app/api": true})
		require.NoError(t, err)
		require.Equal(t, []MeshEdge{
			{Module: "app", Service: "api", Namespace: "example-app", Account: "api", Principal: api,
				Endpoint: "admin", Port: 8082, TargetPort: 8082, Principals: []string{api}},
			{Module: "app", Service: "api", Namespace: "example-app", Account: "api", Principal: api,
				// Its own principal was already admitted here, as a workload of
				// the consumer module: it is admitted once, not twice.
				Endpoint: "grpc", Port: 9090, TargetPort: 9090, Principals: []string{api, worker, gateway}},
		}, edges.Edges)
	})
	t.Run("a service whose render serves everything is unchanged", func(t *testing.T) {
		module, services := moduleServices(t, workspace, "wiki")
		edges, err := deriveMeshEdges(t.Context(), workspace, env, module, services, ports, ports,
			map[string]bool{"wiki/backend": false})
		require.NoError(t, err)
		require.Equal(t, []string{gateway}, edges.Edges[0].Principals)
	})
}

// A database service as its agent renders it: the StatefulSet that serves the
// endpoint, and the bootstrap Job that initializes it under the same account.
const bootstrappedUnit = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: ledger
spec:
  template:
    spec:
      serviceAccountName: ledger
      containers:
        - name: postgres
          image: docker.io/library/postgres@sha256:` + sixtyFourZeros + `
---
apiVersion: batch/v1
kind: Job
metadata:
  name: ledger-bootstrap
spec:
  template:
    spec:
      serviceAccountName: ledger
      containers:
        - name: bootstrap
          image: docker.io/library/postgres@sha256:` + sixtyFourZeros + `
`

const cronJobWorkload = `---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: ledger-vacuum
spec:
  schedule: "0 3 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: ledger
          containers:
            - name: vacuum
              image: docker.io/library/postgres@sha256:` + sixtyFourZeros + `
`

const sixtyFourZeros = "0000000000000000000000000000000000000000000000000000000000000000"

// deriveMeshEdges is handed selfConsuming by the render, which reads it off the
// rendered tree. That reading is the half a composition fixture cannot cover:
// if it stops seeing the Job, every edge below it silently goes back to being
// declared-consumers-only and the bootstrap Job is denied again.
func TestTheRenderReadsSelfConsumptionOffTheRenderedTree(t *testing.T) {
	selfConsumes := func(t *testing.T, manifests string) bool {
		t.Helper()
		unit := writeUnit(t, manifests, nil)
		tree, err := newRenderRoot(unit)
		require.NoError(t, err)
		defer tree.Close()
		consumes, err := renderTreeSelfConsumes(tree, unit, "staging")
		require.NoError(t, err)
		return consumes
	}

	t.Run("a rendered bootstrap Job is a non-serving workload", func(t *testing.T) {
		require.True(t, selfConsumes(t, bootstrappedUnit))
	})
	t.Run("a render that serves everything it holds reaches nothing of its own", func(t *testing.T) {
		serving, _, found := strings.Cut(bootstrappedUnit, "---\n")
		require.True(t, found)
		require.False(t, selfConsumes(t, serving))
	})
	t.Run("a CronJob counts the same as a Job", func(t *testing.T) {
		serving, _, _ := strings.Cut(bootstrappedUnit, "---\n")
		require.True(t, selfConsumes(t, serving+cronJobWorkload))
	})
}
