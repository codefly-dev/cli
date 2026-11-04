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

// hopCellWorkspace is the composition's workspace file, with `prod` carrying
// whatever extra block a case needs — the OPERATOR's half of an admission
// (admitted-sources), an ingress route — indented as a key of that
// environment.
//
// `local` DECLARES a k3d cluster, which is what lifts the hop rule: the
// exception is a developer's own cluster, not a name, so an environment called
// local with no cluster declared is held to the deployed rule like any other.
func hopCellWorkspace(prodExtra string) string {
	return `name: cell
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
` + prodExtra + `  - name: local
    namespace: cell
    cluster:
      kind: k3d
      context: k3d-cell
    host:
      coordinate: cell/local
      component: host
      domain: cell
      audience: mint
      trust_domain: prod.cell
      delivery: authority/hop/http
`
}

// hopCellAdmission is the operator's co-declaration of one admission, as a
// `prod` block for hopCellWorkspace: an admission takes the service's proposal
// and the environment's acceptance, and the render refuses either half alone.
func hopCellAdmission(service, endpoint, caller string) string {
	return "    admitted-sources:\n      - service: " + service + "\n        endpoint: " + endpoint + "\n        from: [" + caller + "]\n"
}

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
		resources.WorkspaceConfigurationName: hopCellWorkspace(""),
		"configurations/prod/hop.env":        "MODULES=platform\n",
		"configurations/local/hop.env":       "MODULES=platform\n",

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

// A SIBLING OF THE HOP IS REFUSED BY NAME, NOT COLLAPSED INTO THE HOP.
//
// The derived allow-list is at module granularity, so a sibling service of the
// hop asking for a fronted module's listener arrives as a principal of the
// hop's module. That ask was once replaced by the hop's own principal, on the
// reasoning that no derivation can tell the two apart — but the hop declares no
// service dependency at all, so every derived principal of its module IS a
// sibling's ask, and the hop's module's own declarers say which service made
// it. The collapse did two wrong things at once: it admitted the hop to a
// listener its route table does not name (where the authority holds no policy
// table for the call), and it deleted the sibling's declared reach, leaving a
// module that fails at runtime with nothing said at render time.
func TestASiblingOfTheHopIsRefusedByName(t *testing.T) {
	workspace, env := hopCell(t, map[string]string{
		"modules/authority/services/local/" + resources.ServiceConfigurationName: "kind: service\nname: local\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\nservice-dependencies:\n  - name: api\n    module: platform\n    endpoints:\n      - name: internal\n",
	})
	_, err := hopCellEdges(t, workspace, env, "platform")
	require.Error(t, err, "the sibling's reach is refused, never silently dropped")
	require.ErrorContains(t, err, "platform/api/internal is published through the hop authority/hop")
	require.ErrorContains(t, err, localPrincipal)
	require.ErrorContains(t, err, "a sibling of the hop in the hop's own module",
		"the refusal says what it is, since the allow-list alone cannot")
	require.ErrorContains(t, err, "service authority/local",
		"and names the declarer that asked, which the hop's module's own declarers can say")
	require.ErrorContains(t, err, "which a module-granular allow-list cannot separate from the hop's",
		"the hop's own principal arrives for the sibling's ask too, and the refusal says whose ask it is rather than reading as the hop being refused its own route")
}

// And the hop itself is admitted only where its route table says so: the
// collapse used to widen it onto listeners the table does not name.
func TestTheHopIsNotAdmittedToAnUnroutedListener(t *testing.T) {
	workspace, env := hopCell(t, map[string]string{
		// The hop service itself declares a dependency on the unrouted
		// listener, so the hop's own principal arrives from the dependency
		// graph rather than from the route table.
		"modules/authority/services/hop/" + resources.ServiceConfigurationName: "kind: service\nname: hop\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nworkspace-configuration-dependencies:\n  - hop-routes\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\nservice-dependencies:\n  - name: api\n    module: platform\n    endpoints:\n      - name: internal\n",
	})
	_, err := hopCellEdges(t, workspace, env, "platform")
	require.ErrorContains(t, err, "the hop's route table does not name this listener",
		"an endpoint the table does not name is not the hop's to reach: the authority holds no policy table for it")
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

// THE EXCEPTION IS A DECLARED DEVELOPER CLUSTER, NOT A NAME. It was keyed on
// core's Local(), which is `strings.HasPrefix(name, "local")`: an environment
// named local-eu, local-shared or localization — with a host declared and
// edges delivered to a real cell — recorded every declared reach and skipped
// the hop refusal entirely. The gate is the one that already decides whether a
// host may admit a developer identity or mount a local trust anchor: a
// DECLARED k3d cluster, because omitting a field must narrow what an
// environment may do, never widen it.
func TestTheLocalExceptionIsADeclaredDeveloperCluster(t *testing.T) {
	named := `  - name: local-eu
    namespace: cell
    host:
      coordinate: cell/local-eu
      component: host
      domain: cell
      audience: mint
      trust_domain: prod.cell
      delivery: authority/hop/http
`
	workspace, _ := hopCell(t, map[string]string{
		resources.WorkspaceConfigurationName:                               hopCellWorkspace(named),
		"configurations/local-eu/hop.env":                                  "MODULES=platform\n",
		"modules/side/services/tool/" + resources.ServiceConfigurationName: toolReachingInternal,
	})
	env, err := environments.Select(workspace, "local-eu")
	require.NoError(t, err)
	require.False(t, env.DeclaresLocalK3d(), "it declares no cluster: only its name says local")
	_, err = hopCellEdges(t, workspace, env, "platform")
	require.ErrorContains(t, err, "platform/api/internal is published through the hop authority/hop",
		"a name that starts with local is not a developer's own cluster")
	require.ErrorContains(t, err, toolPrincipal)
}

// Some listeners are legitimately not called through the hop, and the SERVICE
// says so on the endpoint it serves — but only the OPERATOR can grant it. An
// admission is a waiver of the permission layer, and a waiver written in the
// repository that layer protects is one anyone able to land a commit there
// grants themselves, which is the property standing a hop exists to hold. So
// the manifest proposes and the environment accepts, and every effective
// admission is written into the render record as the waiver set.
//
// It is never implied from a dependency graph either: the edge is what the rule
// refuses, so an edge that admitted itself would admit everything.
func TestAServiceAdmitsAnotherSourceByName(t *testing.T) {
	const manifest = "kind: service\nname: api\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: none\n  - name: internal\n    api: http\n    visibility: internal\nadmitted-sources:\n  - endpoint: internal\n    from: [side/tool]\n"
	admitted := map[string]string{
		resources.WorkspaceConfigurationName:                                  hopCellWorkspace(hopCellAdmission("platform/api", "internal", "side/tool")),
		"modules/platform/services/api/" + resources.ServiceConfigurationName: manifest,
	}
	workspace, env := hopCell(t, admitted)
	edges, err := hopCellEdges(t, workspace, env, "platform")
	require.NoError(t, err)
	internal := hopCellEdge(t, edges, "api", "internal")
	require.NotNil(t, internal, "an admitted caller is an edge even where nothing declares a dependency")
	require.Equal(t, []string{toolPrincipal}, internal.Principals)
	require.Equal(t, []string{hopPrincipal}, hopCellEdge(t, edges, "api", "web").Principals,
		"an endpoint with no entry admits the hop alone")
	require.Equal(t, []MeshAdmission{{
		Module: "platform", Service: "api", Endpoint: "internal",
		Caller: "side/tool", Principal: toolPrincipal,
	}}, edges.Admissions, "the record carries the whole waiver set, so no reviewer reads manifests for it")

	t.Run("the manifest alone grants nothing", func(t *testing.T) {
		workspace, env := hopCell(t, map[string]string{
			"modules/platform/services/api/" + resources.ServiceConfigurationName: manifest,
		})
		_, err := hopCellEdges(t, workspace, env, "platform")
		require.ErrorContains(t, err, "which environment prod does not co-declare")
		require.ErrorContains(t, err, "a waiver written in the module it protects is one its own code grants itself")
		require.ErrorContains(t, err, "- service: platform/api", "the refusal names the entry to add")
	})

	t.Run("and the environment alone admits nobody", func(t *testing.T) {
		workspace, env := hopCell(t, map[string]string{
			resources.WorkspaceConfigurationName: hopCellWorkspace(hopCellAdmission("platform/api", "internal", "side/tool")),
		})
		_, err := hopCellEdges(t, workspace, env, "platform")
		require.ErrorContains(t, err, "environment prod co-declares side/tool admitted to platform/api on endpoint internal")
		require.ErrorContains(t, err, "does not admit it, so the entry admits nobody")
	})

	t.Run("and never on a listener the hop routes", func(t *testing.T) {
		// `web` is the listener the generated route table names.
		workspace, env := hopCell(t, map[string]string{
			resources.WorkspaceConfigurationName:                                  hopCellWorkspace(hopCellAdmission("platform/api", "web", "side/tool")),
			"modules/platform/services/api/" + resources.ServiceConfigurationName: "kind: service\nname: api\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: none\n  - name: internal\n    api: http\n    visibility: internal\nadmitted-sources:\n  - endpoint: web\n    from: [side/tool]\n",
		})
		_, err := hopCellEdges(t, workspace, env, "platform")
		require.ErrorContains(t, err, "which the hop authority/hop routes")
		require.ErrorContains(t, err, "a second door onto the call the hop checks",
			"the composition already said that listener is published through the hop")
	})

	t.Run("and the declaration is refused by name", func(t *testing.T) {
		for _, tc := range []struct{ name, declaration, contains, coDeclared string }{
			{"an endpoint the service does not declare", "  - endpoint: absent\n    from: [side/tool]\n", "endpoint absent is not one this service declares", ""},
			{"a caller the composition does not carry", "  - endpoint: internal\n    from: [absent/tool]\n", "which the composition does not carry", "absent/tool"},
			{"itself", "  - endpoint: internal\n    from: [platform/api]\n", "the service that serves it", ""},
			{"nobody", "  - endpoint: internal\n    from: []\n", "admits nobody", ""},
			{"an empty list", "", "declared and admits nobody", ""},
			{"an endpoint twice", "  - endpoint: internal\n    from: [side/tool]\n  - endpoint: internal\n    from: [exec/runner]\n", "is named twice", ""},
			{"a caller twice", "  - endpoint: internal\n    from: [side/tool, side/tool]\n", "admits a caller twice", ""},
			{"an unknown key", "  - endpoint: internal\n    allow: [side/tool]\n", "it is a list of {endpoint, from} entries", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// The operator's half names the same caller, so each case
				// fails on the defect it is about rather than on a missing
				// co-declaration.
				caller := tc.coDeclared
				if caller == "" {
					caller = "side/tool"
				}
				broken, brokenEnv := hopCell(t, map[string]string{
					resources.WorkspaceConfigurationName:                                  hopCellWorkspace(hopCellAdmission("platform/api", "internal", caller)),
					"modules/platform/services/api/" + resources.ServiceConfigurationName: "kind: service\nname: api\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: none\n  - name: internal\n    api: http\n    visibility: internal\nadmitted-sources:\n" + tc.declaration,
				})
				_, err := hopCellEdges(t, broken, brokenEnv, "platform")
				require.ErrorContains(t, err, tc.contains)
			})
		}
	})

	// An environment that EXCLUDES or manages the admitted caller must not
	// break the render of the module that admits it: a derived source in the
	// same state is skipped silently, and refusing one here meant
	// `exclude-services` on a dev environment failed an unrelated module's
	// render. The edge is omitted, and the record says why.
	t.Run("a caller this environment does not run omits the edge and says so", func(t *testing.T) {
		excluded := `    exclude-services:
      - service: side/tool
`
		workspace, env := hopCell(t, map[string]string{
			resources.WorkspaceConfigurationName:                                  hopCellWorkspace(hopCellAdmission("platform/api", "internal", "side/tool") + excluded),
			"modules/platform/services/api/" + resources.ServiceConfigurationName: manifest,
		})
		edges, err := hopCellEdges(t, workspace, env, "platform")
		require.NoError(t, err, "excluding a caller is not a defect in the module that admits it")
		require.Nil(t, hopCellEdge(t, edges, "api", "internal"), "nothing reaches it here, and default deny is the declaration")
		require.Len(t, edges.Admissions, 1)
		require.Empty(t, edges.Admissions[0].Principal)
		require.Contains(t, edges.Admissions[0].Omitted, "runs no workload for it")
	})
}

// AN ADMISSION APPLIES WHEREVER IT IS DECLARED AND ACCEPTED, HOP OR NONE.
//
// It was read and validated for every service of every environment but
// APPLIED only behind a hop on a non-local one: a module outside hop/MODULES,
// or one taken out of it, silently lost its admitted callers and was denied at
// the mesh with every declaration in the composition saying it was allowed. A
// security declaration is never validate-and-drop.
func TestAnAdmissionAppliesWithNoHopInFront(t *testing.T) {
	// `side` is not behind the hop, and nothing declares a dependency on its
	// tool, so the admission is the only thing that can produce the edge.
	workspace, env := hopCell(t, map[string]string{
		resources.WorkspaceConfigurationName:                               hopCellWorkspace(hopCellAdmission("side/tool", "probe", "exec/runner")),
		"modules/side/services/tool/" + resources.ServiceConfigurationName: "kind: service\nname: tool\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: probe\n    api: http\n    visibility: internal\nadmitted-sources:\n  - endpoint: probe\n    from: [exec/runner]\n",
	})
	loaded, services := moduleServices(t, workspace, "side")
	ports := map[string]map[string]uint32{"side/tool": {"probe": 9000}}
	edges, err := deriveMeshEdges(t.Context(), workspace, env, loaded, services, ports, ports, nil)
	require.NoError(t, err)
	probe := hopCellEdge(t, edges, "tool", "probe")
	require.NotNil(t, probe, "the admission is applied, not validated and dropped")
	require.Equal(t, []string{runnerPrinc}, probe.Principals)
	require.Len(t, edges.Admissions, 1)
}

// An ingress route is the cell's own edge rather than a principal, which is
// why the hop rule otherwise leaves it alone — but the edge is reachable from
// inside the cell too, so a route onto a listener the hop ROUTES is a hairpin
// around the hop that needs only a Host header.
func TestAnIngressRouteOntoAHopRoutedListenerIsRefused(t *testing.T) {
	route := `    ingress:
      - name: web
        service: platform/api
        endpoint: web
        hosts: [api.cell]
`
	workspace, env := hopCell(t, map[string]string{
		resources.WorkspaceConfigurationName:                                  hopCellWorkspace(route),
		"modules/platform/services/api/" + resources.ServiceConfigurationName: "kind: service\nname: api\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nendpoints:\n  - name: web\n    api: http\n    visibility: public\n    exposure: public\n  - name: internal\n    api: http\n    visibility: internal\n",
	})
	_, err := hopCellEdges(t, workspace, env, "platform")
	require.ErrorContains(t, err, "but the hop authority/hop routes that listener")
	require.ErrorContains(t, err, "with a Host header and never through the hop")
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

// A service the environment WITHDRAWS stands nothing. blue withdraws
// authority/hop — on a cell saas is the host and its gateway plays the hop
// role — and counting the withdrawn service made the cell refuse its own
// render with "authority/hop and saas/auth-gateway both declare hop-routes",
// naming a service the same render had already skipped.
func TestAWithdrawnServiceDoesNotStandTheHop(t *testing.T) {
	second := map[string]string{
		"modules/authority/services/local/" + resources.ServiceConfigurationName: "kind: service\nname: local\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\nworkspace-configuration-dependencies:\n  - hop-routes\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\n",
	}

	// Without the withdrawal the two collide: that is the refusal this test
	// exists beside, and asserting it here keeps the fixture honest.
	workspace, env := hopCell(t, second)
	_, err := hopCellEdges(t, workspace, env, "platform")
	require.ErrorContains(t, err, "both declare hop-routes")

	// Withdrawing the second leaves exactly one standing, and the render runs.
	workspace, env = hopCell(t, second)
	env.ExcludeServices = append(env.ExcludeServices,
		environments.EnvironmentExcludedService{Service: "authority/local"})
	_, err = hopCellEdges(t, workspace, env, "platform")
	require.NoError(t, err)
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
