package gitops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// hopCell composes what a cell behind a hop is: an `authority` module whose
// `hop` service stands the hop (it declares hop-routes) beside a `local`
// sibling, a `platform` module published through it (hop/MODULES), an `exec`
// module whose runner installs prepared runnable bindings, and a `side` module
// whose tool is the direct consumer the rule exists to refuse.
//
// platform publishes one OpenAPI listener, so the generated route table names
// `platform/api/web` and nothing else of it: `platform/api/internal` is a
// listener the hop does not route, and `platform/worker` serves none at all.
//
// extra lets a case add or replace one file — a direct dependency, an
// admitted-sources declaration — without a second copy of the composition.
func hopCell(t *testing.T, extra map[string]string) (*resources.Workspace, *environments.Environment) {
	t.Helper()
	service := func(name, rest string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n" + rest
	}
	const doc = `{"openapi":"3.0.0","info":{"title":"api","version":"1.0.0"},"paths":{"/things":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	digest := composition.APIContractDigest([]byte(doc))
	catalog := composition.APIContractCatalog{
		Schema: composition.APIContractCatalogSchema, Package: "owner/platform", Version: "1.0.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service: "api", Endpoint: "web", API: "http", Kind: composition.APIContractKindOpenAPI,
			Package: "api", Path: "contracts/api/api.json", Digest: digest,
			Routes: []composition.APIContractRoute{{Method: "GET", Path: "/things"}},
		}},
	}
	encoded, err := json.Marshal(catalog)
	require.NoError(t, err)
	authorityCatalog, err := json.Marshal(composition.APIContractCatalog{
		Schema: composition.APIContractCatalogSchema, Package: "owner/authority", Version: "1.0.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service: "hop", Endpoint: "http", API: "http", Kind: composition.APIContractKindOpenAPI,
			Package: "api", Path: "contracts/api/api.json", Digest: digest,
			Routes: []composition.APIContractRoute{{Method: "GET", Path: "/things"}},
		}},
	})
	require.NoError(t, err)

	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: cell
layout: modules
modules:
  - name: authority
  - name: platform
  - name: exec
  - name: side
environments:
  - name: prod
    namespace: cell
    host:
      coordinate: cell/prod
      component: host
      domain: cell
      audience: mint
      trust_domain: prod.cell
      delivery: authority/hop/http
  - name: local
    namespace: cell
    host:
      coordinate: cell/local
      component: host
      domain: cell
      audience: mint
      trust_domain: prod.cell
      delivery: authority/hop/http
`,
		"configurations/prod/hop.env":  "MODULES=platform\n",
		"configurations/local/hop.env": "MODULES=platform\n",

		"modules/authority/" + resources.ModuleConfigurationName:      "kind: module\nname: authority\nservices:\n  - name: hop\n  - name: local\n",
		"modules/authority/module.contract.codefly.yaml":              "schema: codefly/module-contract/v1\nprincipal: authority\nnamespaces: [authority]\nqueues: []\nscope_ceilings: []\nbindings: []\n",
		"modules/authority/contracts/api/api.json":                    doc,
		"modules/authority/" + composition.APIContractCatalogFileName: string(authorityCatalog),
		"modules/authority/module.package.codefly.yaml":               "schema: codefly/module-package/v2\nkind: module-package\nid: owner/authority\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n - name: hop\n   endpoints: [http]\n   api-contracts:\n    - endpoint: http\n      kind: openapi\n      package: api\n      path: contracts/api/api.json\n      digest: " + digest + "\n",
		"modules/authority/services/hop/" + resources.ServiceConfigurationName: service("hop",
			"workspace-configuration-dependencies:\n  - hop-routes\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\n"),
		"modules/authority/services/local/" + resources.ServiceConfigurationName: service("local",
			"endpoints:\n  - name: http\n    api: http\n    visibility: internal\n"),

		"modules/platform/" + resources.ModuleConfigurationName:      "kind: module\nname: platform\nservices:\n  - name: api\n  - name: worker\n",
		"modules/platform/module.contract.codefly.yaml":              "schema: codefly/module-contract/v1\nprincipal: platform\nnamespaces: [platform]\nqueues: []\nscope_ceilings: []\nbindings: []\n",
		"modules/platform/contracts/api/api.json":                    doc,
		"modules/platform/" + composition.APIContractCatalogFileName: string(encoded),
		"modules/platform/module.package.codefly.yaml":               "schema: codefly/module-package/v2\nkind: module-package\nid: owner/platform\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n - name: api\n   endpoints: [web]\n   api-contracts:\n    - endpoint: web\n      kind: openapi\n      package: api\n      path: contracts/api/api.json\n      digest: " + digest + "\n",
		"modules/platform/services/api/" + resources.ServiceConfigurationName: service("api",
			"endpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: none\n  - name: internal\n    api: http\n    visibility: internal\n"),
		"modules/platform/services/worker/" + resources.ServiceConfigurationName: service("worker", ""),

		"modules/exec/" + resources.ModuleConfigurationName: "kind: module\nname: exec\nservices:\n  - name: runner\n",
		"modules/exec/services/runner/" + resources.ServiceConfigurationName: service("runner",
			"workspace-configuration-dependencies:\n  - runnable-bindings\n"),

		"modules/side/" + resources.ModuleConfigurationName:                "kind: module\nname: side\nservices:\n  - name: tool\n",
		"modules/side/services/tool/" + resources.ServiceConfigurationName: service("tool", ""),
	}
	for name, content := range extra {
		files[name] = content
	}
	root := t.TempDir()
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

// hopCellPorts is every endpoint of the composition at an in-cluster port, so
// a case that narrows an edge is not quietly passing because the endpoint had
// no port.
// toolReachingInternal is side/tool with a direct dependency on a module
// listener behind the hop. It names api's INTERNAL endpoint: a public
// endpoint's derived allow-list is nil by design ("public is open"), so a
// declared direct reach does not show up there at all.
const toolReachingInternal = "kind: service\nname: tool\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nservice-dependencies:\n  - name: api\n    module: platform\n    endpoints:\n      - name: internal\n"

func hopCellPorts() map[string]map[string]uint32 {
	return map[string]map[string]uint32{
		"authority/hop":   {"http": 8080},
		"authority/local": {"http": 8080},
		"platform/api":    {"web": 8080, "internal": 8081},
	}
}

func hopCellEdges(t *testing.T, workspace *resources.Workspace, env *environments.Environment, module string) (*MeshEdges, error) {
	t.Helper()
	loaded, services := moduleServices(t, workspace, module)
	ports := hopCellPorts()
	return deriveMeshEdges(t.Context(), workspace, env, loaded, services, ports, ports, nil)
}

func hopCellEdge(t *testing.T, edges *MeshEdges, service, endpoint string) *MeshEdge {
	t.Helper()
	for index := range edges.Edges {
		if edges.Edges[index].Service == service && edges.Edges[index].Endpoint == endpoint {
			return &edges.Edges[index]
		}
	}
	return nil
}

const (
	hopPrincipal   = "spiffe://prod.cell/ns/cell-authority/sa/hop"
	localPrincipal = "spiffe://prod.cell/ns/cell-authority/sa/local"
	toolPrincipal  = "spiffe://prod.cell/ns/cell-side/sa/tool"
	runnerPrinc    = "spiffe://prod.cell/ns/cell-exec/sa/runner"
)

// The whole point of standing a hop is that a module's code never has to
// install the check, so its listeners must have no other way to receive
// traffic: the listener the hop routes admits the HOP's principal, and one it
// does not route admits nobody at all.
func TestAModuleBehindTheHopAdmitsOnlyItsHop(t *testing.T) {
	workspace, env := hopCell(t, nil)
	edges, err := hopCellEdges(t, workspace, env, "platform")
	require.NoError(t, err)
	routed := hopCellEdge(t, edges, "api", "web")
	require.NotNil(t, routed, "the listener the hop's route table names has an edge")
	require.Equal(t, []string{hopPrincipal}, routed.Principals)
	require.Nil(t, hopCellEdge(t, edges, "api", "internal"),
		"a listener the hop does not route is reached by nobody, and default deny is the declaration")
}

// A sibling service of the hop's own module asks at MODULE granularity, which
// no derivation can tell from the hop's own ask — so it is narrowed to the hop
// rather than refused, and the sibling loses the reach it declared.
func TestASiblingOfTheHopIsNarrowedToTheHop(t *testing.T) {
	workspace, env := hopCell(t, map[string]string{
		"modules/authority/services/local/" + resources.ServiceConfigurationName: "kind: service\nname: local\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\nservice-dependencies:\n  - name: api\n    module: platform\n    endpoints:\n      - name: internal\n",
	})
	edges, err := hopCellEdges(t, workspace, env, "platform")
	require.NoError(t, err)
	internal := hopCellEdge(t, edges, "api", "internal")
	require.NotNil(t, internal)
	require.Equal(t, []string{hopPrincipal}, internal.Principals, "the hop, never the sibling")
	require.NotContains(t, internal.Principals, localPrincipal)
}

// A composition that routes a consumer straight to a module listener is
// refused outside local, naming the consumer and what to change. Inside local
// the reach is recorded as declared: a developer's own cluster is where
// calling a module directly is the point.
func TestADirectConsumerOfAModuleBehindTheHopIsRefused(t *testing.T) {
	direct := map[string]string{
		"modules/side/services/tool/" + resources.ServiceConfigurationName: toolReachingInternal,
	}
	workspace, env := hopCell(t, direct)
	_, err := hopCellEdges(t, workspace, env, "platform")
	require.Error(t, err)
	require.ErrorContains(t, err, "platform/api/internal is published through the hop authority/hop")
	require.ErrorContains(t, err, toolPrincipal)
	require.ErrorContains(t, err, "module side declares a run-stage dependency")
	require.ErrorContains(t, err, admittedSourcesKey)

	local, err := environments.Select(workspace, "local")
	require.NoError(t, err)
	edges, err := hopCellEdges(t, workspace, local, "platform")
	require.NoError(t, err, "a local cluster keeps the reach the composition declares")
	require.Contains(t, hopCellEdge(t, edges, "api", "internal").Principals, toolPrincipal)
}

// Some listeners are legitimately not called through the hop, and the SERVICE
// says so on the endpoint it serves. It is never implied from a dependency
// graph: the edge is what the rule refuses, so an edge that admitted itself
// would admit everything.
func TestAServiceAdmitsAnotherSourceByName(t *testing.T) {
	const manifest = "kind: service\nname: api\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: none\n  - name: internal\n    api: http\n    visibility: internal\nadmitted-sources:\n  - endpoint: internal\n    from: [side/tool]\n"
	workspace, env := hopCell(t, map[string]string{
		"modules/platform/services/api/" + resources.ServiceConfigurationName: manifest,
	})
	edges, err := hopCellEdges(t, workspace, env, "platform")
	require.NoError(t, err)
	internal := hopCellEdge(t, edges, "api", "internal")
	require.NotNil(t, internal, "an admitted caller is an edge even where nothing declares a dependency")
	require.Equal(t, []string{toolPrincipal}, internal.Principals)
	require.Equal(t, []string{hopPrincipal}, hopCellEdge(t, edges, "api", "web").Principals,
		"an endpoint with no entry admits the hop alone")

	t.Run("and the declaration is refused by name", func(t *testing.T) {
		for _, tc := range []struct{ name, declaration, contains string }{
			{"an endpoint the service does not declare", "  - endpoint: absent\n    from: [side/tool]\n", "endpoint absent is not one this service declares"},
			{"a caller the composition does not carry", "  - endpoint: internal\n    from: [absent/tool]\n", "runs no workload for"},
			{"itself", "  - endpoint: internal\n    from: [platform/api]\n", "the service that serves it"},
			{"nobody", "  - endpoint: internal\n    from: []\n", "admits nobody"},
			{"an empty list", "", "declared and admits nobody"},
			{"an endpoint twice", "  - endpoint: internal\n    from: [side/tool]\n  - endpoint: internal\n    from: [exec/runner]\n", "is named twice"},
			{"a caller twice", "  - endpoint: internal\n    from: [side/tool, side/tool]\n", "admits a caller twice"},
			{"an unknown key", "  - endpoint: internal\n    allow: [side/tool]\n", "it is a list of {endpoint, from} entries"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				broken, brokenEnv := hopCell(t, map[string]string{
					"modules/platform/services/api/" + resources.ServiceConfigurationName: "kind: service\nname: api\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: none\n  - name: internal\n    api: http\n    visibility: internal\nadmitted-sources:\n" + tc.declaration,
				})
				_, err := hopCellEdges(t, broken, brokenEnv, "platform")
				require.ErrorContains(t, err, tc.contains)
			})
		}
	})
}

// A prepared runnable call is sent where the environment says: to the hop,
// which forwards it, or straight to the owner's listener. The second routes
// around the hop and is refused outside local; the first admits the executor
// at the HOP's endpoint, which is what the mesh then has to allow.
func TestPreparedRunnableCallsGoThroughTheHop(t *testing.T) {
	const operations = `{"schema":"codefly/runnable-operations/v1","workspace":"cell","module":"platform","operations":[{"name":"do","version":"0.0.1","digest":"sha256:0","service":"api","endpoint":"web","method":"Do","input_message":"DoRequest","output_message":"DoResponse","path":"do"}]}`
	workspace, env := hopCell(t, map[string]string{"modules/platform/contracts/runnables/index.json": operations})

	_, err := hopCellEdges(t, workspace, env, "platform")
	require.ErrorContains(t, err, "platform/api/web is published through the hop authority/hop")
	require.ErrorContains(t, err, runnerPrinc)
	require.ErrorContains(t, err, "installs prepared runnable bindings")
	require.ErrorContains(t, err, "runnable-call-endpoint")

	through := *env
	through.RunnableCallEndpoint = "authority/hop/http"
	edges, err := hopCellEdges(t, workspace, &through, "platform")
	require.NoError(t, err)
	require.Equal(t, []string{hopPrincipal}, hopCellEdge(t, edges, "api", "web").Principals,
		"the owner's listener is still reached by the hop alone")

	hopEdges, err := hopCellEdges(t, workspace, &through, "authority")
	require.NoError(t, err)
	require.Contains(t, hopCellEdge(t, hopEdges, "hop", "http").Principals, runnerPrinc,
		"the executor reaches the endpoint the prepared calls are sent to")
}

// The hop is derived from the composition's own two statements, and a
// composition whose halves disagree is refused by name rather than rendered
// with a reach nothing checks.
func TestTheCompositionsHopIsRefusedWhenItsHalvesDisagree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    map[string]string
		contains string
	}{
		{
			"modules named for a hop nothing stands",
			map[string]string{"modules/authority/services/hop/" + resources.ServiceConfigurationName: "kind: service\nname: hop\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\n"},
			"no service of the composition declares hop-routes",
		},
		{
			"two services standing one hop",
			map[string]string{"modules/authority/services/local/" + resources.ServiceConfigurationName: "kind: service\nname: local\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nworkspace-configuration-dependencies:\n  - hop-routes\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\n"},
			"both declare hop-routes",
		},
		{
			// Refused where the route table is generated, which this
			// derivation deliberately does not restate.
			"a module the composition does not carry",
			map[string]string{"configurations/prod/hop.env": "MODULES=platform,absent\n"},
			"hop routes for absent",
		},
		{
			"the hop's own module",
			map[string]string{"configurations/prod/hop.env": "MODULES=authority\n"},
			"the hop is reached directly",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, env := hopCell(t, tc.files)
			_, err := hopCellEdges(t, workspace, env, "platform")
			require.ErrorContains(t, err, tc.contains)
		})
	}
}

// A composition with no hop is unchanged: nothing is narrowed and nothing is
// refused, because there is no door to route around.
func TestACompositionWithNoHopIsUnchanged(t *testing.T) {
	workspace, env := hopCell(t, map[string]string{
		"configurations/prod/hop.env":                                      "",
		"modules/side/services/tool/" + resources.ServiceConfigurationName: toolReachingInternal,
	})
	edges, err := hopCellEdges(t, workspace, env, "platform")
	require.NoError(t, err)
	require.Equal(t, []string{toolPrincipal}, hopCellEdge(t, edges, "api", "internal").Principals)
}
