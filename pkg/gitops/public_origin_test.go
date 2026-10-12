package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const frontendOriginKey = "CODEFLY__PUBLIC_ORIGIN__PAYMENTS__FRONTEND__HTTP__HTTP"

// exposedFrontend serves one endpoint ALLOCATED an outward address, one
// reachable from outside the workspace but allocated none, and one the module
// keeps to itself. Only the first is asked for an origin.
func exposedFrontend() *resources.Service {
	return &resources.Service{
		Name: "frontend",
		Endpoints: []*resources.Endpoint{
			{Name: "http", API: "http", Visibility: resources.VisibilityPublic, Exposure: resources.ExposurePublic},
			{Name: "rest", API: "rest", Visibility: resources.VisibilityPublic, Exposure: resources.ExposureNone},
			{Name: "grpc", API: "grpc", Visibility: resources.VisibilityInternal},
		},
	}
}

func cellEnvironment() *environments.Environment {
	return &environments.Environment{Name: "staging", Namespace: "payments"}
}

func productRoute(hosts ...string) environments.EnvironmentIngressRoute {
	return environments.EnvironmentIngressRoute{Name: "product", Service: "frontend", Endpoint: "http", Hosts: hosts}
}

// rewriteFile changes a rendered file, which is how these tests express what an
// agent or an operator did to the tree.
func rewriteFile(t *testing.T, path string, replace func(string) string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(replace(string(data))), 0o644))
}

func boundaryWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), filepath.Join("..", "solutionrun", "testdata", "solution-boundary"))
	require.NoError(t, err)
	return workspace
}

// The origin a service answers as is operator configuration, so it is the
// environment's declaration that supplies it — and only for the endpoint the
// route names.
func TestPublicOriginCarrierComesFromTheIngressDeclaration(t *testing.T) {
	env := cellEnvironment()
	env.Ingress = []environments.EnvironmentIngressRoute{productRoute("app.example.com", "www.example.com")}

	carriers, err := publicOriginCarriers("payments", exposedFrontend(), env)
	require.NoError(t, err)
	require.Equal(t, map[string]string{frontendOriginKey: "https://app.example.com"}, carriers,
		"the first declared host is the canonical origin, and a module-visibility endpoint has no public origin at all")
}

// With no route, the environment's declared app host suffix is the declaration,
// derived exactly as an external endpoint's DNS record is.
func TestPublicOriginCarrierFallsBackToTheAppHostSuffix(t *testing.T) {
	env := cellEnvironment()
	env.DNS = &environments.EnvironmentDNS{AppHostSuffix: "cell.example.com"}

	carriers, err := publicOriginCarriers("payments", exposedFrontend(), env)
	require.NoError(t, err)
	require.Equal(t, map[string]string{frontendOriginKey: "https://frontend-payments.cell.example.com"}, carriers)
}

// A declaration that exists but resolves to nothing refuses exactly like no
// declaration at all. Presence was never the question: a workload is handed an
// origin or it is not.
func TestDeployedRenderRefusesAnUnresolvedDeclaration(t *testing.T) {
	for name, route := range map[string]environments.EnvironmentIngressRoute{
		"nothing declared": {},
		"an empty route":   {Name: "product"},
		"another service":  {Name: "product", Service: "absent", Hosts: []string{"app.example.com"}},
		"another endpoint": {Name: "product", Service: "payments/frontend", Endpoint: "absent", Hosts: []string{"app.example.com"}},
		"no hosts":         {Name: "product", Service: "payments/frontend", Endpoint: "http"},
		"a blank host":     {Name: "product", Service: "payments/frontend", Endpoint: "http", Hosts: []string{" "}},
		"an unusable host": {Name: "product", Service: "payments/frontend", Endpoint: "http", Hosts: []string{"localhost"}},
	} {
		t.Run(name, func(t *testing.T) {
			env := cellEnvironment()
			env.Ingress = []environments.EnvironmentIngressRoute{route}
			_, err := publicOriginCarriers("payments", exposedFrontend(), env)
			require.Error(t, err)
			require.Contains(t, err.Error(), "payments/frontend/http")
		})
	}
}

// A local cluster is one machine, and the origin a developer's browser uses to
// reach it IS a loopback one — so the declaration is carried, and nothing is
// refused. That is the whole reason "is this a well-formed origin" and "is this
// a public origin" are two questions: the second is a cell's, and asking it here
// would refuse a local composition for declaring the truth about itself.
func TestLocalRenderCarriesItsOwnOriginAndRefusesNothing(t *testing.T) {
	local := &environments.Environment{
		Name:    "local",
		Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d},
		Ingress: []environments.EnvironmentIngressRoute{productRoute("app.shop.localhost")},
	}
	carriers, err := publicOriginCarriers("payments", exposedFrontend(), local)
	require.NoError(t, err)
	require.Equal(t, map[string]string{frontendOriginKey: "https://app.shop.localhost"}, carriers)

	// The same host on a cell is refused, naming the endpoint and the reason.
	cell := cellEnvironment()
	cell.Ingress = []environments.EnvironmentIngressRoute{productRoute("app.shop.localhost")}
	_, err = publicOriginCarriers("payments", exposedFrontend(), cell)
	require.Error(t, err)
	require.Contains(t, err.Error(), "payments/frontend/http")
	require.Contains(t, err.Error(), "names the machine the workload runs on")

	// And a composition that declares nothing at all is not refused locally.
	bare := &environments.Environment{Name: "local", Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d}}
	carriers, err = publicOriginCarriers("payments", exposedFrontend(), bare)
	require.NoError(t, err)
	require.Empty(t, carriers)
}

// An endpoint that lives outside the system is not served by this workload, so
// it has no origin of its own and is never refused for having none.
func TestPublicOriginIgnoresAnExternalEndpoint(t *testing.T) {
	service := &resources.Service{
		Name: "payments-gateway",
		Endpoints: []*resources.Endpoint{
			{Name: "rest", API: "rest", Visibility: resources.VisibilityPublic, Location: resources.LocationExternal},
		},
	}
	carriers, err := publicOriginCarriers("payments", service, cellEnvironment())
	require.NoError(t, err)
	require.Empty(t, carriers)
}

// The composition-wide pass reports every exposed endpoint at once, so an
// operator writes one set of declarations rather than discovering them one
// render at a time — and an endpoint reachable from outside the workspace but
// allocated no address of its own is not among them.
func TestDeployedRenderReportsEveryUnresolvedExposedEndpoint(t *testing.T) {
	workspace := boundaryWorkspace(t)
	err := requirePublicOriginsFixed(t.Context(), workspace, cellEnvironment())
	require.Error(t, err)
	require.Contains(t, err.Error(), "host/frontend/http", "the host's front door is addressed from outside")
	require.NotContains(t, err.Error(), "consumer/backend",
		"the guest is reachable from outside the workspace but allocated no address of its own")

	fixed := cellEnvironment()
	fixed.Ingress = []environments.EnvironmentIngressRoute{
		{Name: "product", Service: "host/frontend", Endpoint: "http", Hosts: []string{"app.example.com"}},
	}
	require.NoError(t, requirePublicOriginsFixed(t.Context(), workspace, fixed))

	suffixed := cellEnvironment()
	suffixed.DNS = &environments.EnvironmentDNS{AppHostSuffix: "cell.example.com"}
	require.NoError(t, requirePublicOriginsFixed(t.Context(), workspace, suffixed))

	local := &environments.Environment{Name: "local", Cluster: &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d}}
	require.NoError(t, requirePublicOriginsFixed(t.Context(), workspace, local))
}

// An endpoint reachable from outside the workspace but allocated no address is
// never asked for an origin, and never refused for having none. A visibility is
// reach; exposure is addressing.
func TestReachWithoutAnAddressIsNotAsked(t *testing.T) {
	reachOnly := &resources.Service{
		Name: "annotations",
		Endpoints: []*resources.Endpoint{
			{Name: "rest", API: "rest", Visibility: resources.VisibilityPublic, Exposure: resources.ExposureNone},
		},
	}
	carriers, err := publicOriginCarriers("payments", reachOnly, cellEnvironment())
	require.NoError(t, err)
	require.Empty(t, carriers)
}

// The render has to call the precondition, not merely have one: removing the
// call left every helper test green.
func TestRenderCallsTheOriginPrecondition(t *testing.T) {
	_, err := deriveRenderInjections(t.Context(), boundaryWorkspace(t), cellEnvironment(), nil, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fixes no public origin")
	require.Contains(t, err.Error(), "host/frontend/http")
}

// Through the real projection: the origin reaches the workload in the ConfigMap
// its container already loads its configuration from.
func TestPublicOriginReachesTheRenderedConfigMap(t *testing.T) {
	env := storeEnvironment()
	env.Ingress = []environments.EnvironmentIngressRoute{productRoute("app.example.com")}
	service := exposedFrontend()
	scope := scopeOf(env)
	scope.Module = "payments"

	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, service.Name, "documents.example.test:8080")
	require.NoError(t, projectServiceConfiguration(t.Context(), root, service, env, scope, serviceInjection{}))
	require.Equal(t, "https://app.example.com", configMapData(t, buildOverlay(t, root, env.Name))[frontendOriginKey])
}

// A declared origin is required delivery, not an offer. An overlay that unbinds
// the configuration the workload loads, and a container that reads none, each
// leave the declaration undelivered — so the render refuses by name instead of
// skipping it.
func TestPublicOriginMustReachTheEffectiveWorkload(t *testing.T) {
	for name, shape := range map[string]string{
		"the overlay unbinds the configuration":                 "patches:\n  - target:\n      kind: Deployment\n      name: frontend\n    patch: |-\n      - op: remove\n        path: /spec/template/spec/containers/0/envFrom\n",
		"the overlay unbinds it but keeps the service identity": "patches:\n  - target:\n      kind: Deployment\n      name: frontend\n    patch: |-\n      - op: remove\n        path: /spec/template/spec/containers/0/envFrom\n      - op: add\n        path: /spec/template/spec/containers/0/env\n        value:\n          - name: CODEFLY__SERVICE\n            value: frontend\n",
	} {
		t.Run(name, func(t *testing.T) {
			env := storeEnvironment()
			env.Ingress = []environments.EnvironmentIngressRoute{productRoute("app.example.com")}
			scope := scopeOf(env)
			scope.Module = "payments"
			root := t.TempDir()
			writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "documents.example.test:8080")
			patchOverlay(t, root, env.Name, shape)

			err := projectServiceConfiguration(t.Context(), root, exposedFrontend(), env, scope, serviceInjection{})
			require.Error(t, err, "the declared origin no longer reaches the workload")
		})
	}
}

// And the same when nothing in the rendered unit claims the service at all: a
// workload that reads no Codefly configuration cannot be handed the origin an
// operator declared for it, so the contradiction is refused rather than skipped.
func TestPublicOriginRefusesAWorkloadThatClaimsNoService(t *testing.T) {
	env := storeEnvironment()
	env.Ingress = []environments.EnvironmentIngressRoute{productRoute("app.example.com")}
	scope := scopeOf(env)
	scope.Module = "payments"
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, "frontend", "documents.example.test:8080")
	rewriteFile(t, filepath.Join(root, "base", "config-map.yaml"), func(text string) string {
		return strings.ReplaceAll(text, "  CODEFLY__SERVICE: \"frontend\"\n", "")
	})

	err := projectServiceConfiguration(t.Context(), root, exposedFrontend(), env, scope, serviceInjection{})
	require.Error(t, err)
}
