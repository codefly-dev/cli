package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	solutionmanifest "github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// consumingSolution consumes a platform module with the viewer's own
// credential, a documents module it calls on a person's behalf, and a chat
// module it does not.
const consumingSolution = `schema_version: codefly.solution-manifest/v1
protocol_version: codefly.solution/v0
agent:
  publisher: codefly.dev
  kind: codefly:solution
  name: solution-generic
  version: 0.0.1
api:
  exposes:
    - id: http
      protocol: http
  consumes:
    - id: identity
      protocol: connect
      module: platform
      service: identity
      endpoint: connect
      as: identity
    - id: documents
      protocol: rest
      module: documents
      service: documents
      endpoint: rest
      as: documents
      audience: {from: documents/documents-prefix}
      scopes:
        - resource_kind: documents.passages
          actions: [read]
        - resource_kind: documents.collections
          actions: [read]
    - id: chat
      protocol: http
      module: assistant
      service: chat
      endpoint: http
      as: assistant
lifecycle:
  package: true
  render: true
`

// writeConsumingSolutionWorkspace is writeAuthorityWorkspace with the module's
// contract replaced by a solution manifest, composing the modules a scoped
// consumption asks for authority at (documents, model) and supplying the
// audience each verifies.
func writeConsumingSolutionWorkspace(t *testing.T, solution string) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace, module := writeAuthorityWorkspace(t)
	root := workspace.Dir()
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), modulecontract.FileName)))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), solutionmanifest.FileName), []byte(solution), 0o644))
	config := filepath.Join(root, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	composed := strings.Replace(string(data), "  - name: billing\n", "  - name: billing\n  - name: documents\n  - name: model\n", 1)
	require.NotEqual(t, string(data), composed)
	require.NoError(t, os.WriteFile(config, []byte(composed), 0o644))
	for _, name := range []string{"documents", "model"} {
		dir := filepath.Join(root, "modules", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, resources.ModuleConfigurationName), []byte("kind: module\nname: "+name+"\n"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "configurations", "staging", "documents.env"), []byte("DOCUMENTS_PREFIX=documents\n"), 0o644))
	ctx := context.Background()
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	reloadedModule, err := reloaded.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return reloaded, reloadedModule
}

func consumptionFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "consumption", name))
	require.NoError(t, err)
	return string(data)
}

func deriveShopAuthority(t *testing.T, solution string) (*solutionhost.Authority, error) {
	t.Helper()
	workspace, module := writeConsumingSolutionWorkspace(t, solution)
	env := selectedEnvironment(t, workspace, "staging")
	return shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
}

// A solution's declared consumption becomes its manifest's requested binding
// for the module it calls on a person's behalf — its audience resolved from
// the composition, bounded by exactly the declared scopes — and the
// consumptions that ask for no scopes become nothing.
func TestRenderDerivesAuthorityFromASolutionsDeclaredConsumption(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	env := selectedEnvironment(t, workspace, "staging")
	_, manifest := renderShopManifest(t, workspace, module, env)
	require.Equal(t, &solutionhost.Authority{
		Principal: "shop",
		Bindings: []solutionhost.AuthorityBinding{{ID: "documents", Audience: "documents", Operations: map[string][]string{
			"invoke": {"documents.collections:read", "documents.passages:read"},
		}}},
	}, manifest.Authority, "one binding, for the one consumption that asks for scopes, under the name the solution is composed under")
}

// The scoped consumer fixture renders one documents binding end to end, and
// deterministically: a second render is byte-identical.
func TestAScopedConsumerRendersDeterministically(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumptionFixture(t, "scoped-consumer.yaml"))
	env := selectedEnvironment(t, workspace, "staging")
	destination, manifest := renderShopManifest(t, workspace, module, env)
	require.Equal(t, []solutionhost.AuthorityBinding{{ID: "documents", Audience: "documents", Operations: map[string][]string{"invoke": {"documents:read"}}}}, manifest.Authority.Bindings)

	path := filepath.Join(destination, filepath.FromSlash(moduleManifestOverlay("staging")), "acme.staging.shop.yaml")
	first, err := os.ReadFile(path)
	require.NoError(t, err)
	renderShopManifest(t, workspace, module, env)
	second, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

// The audience follows the composition, as a contract's does: the composition
// says the model module verifies model-gateway, and that is the audience.
func TestTheConsumptionAudienceFollowsTheComposition(t *testing.T) {
	model := strings.Replace(consumingSolution, "    - id: chat\n",
		"    - id: model\n      protocol: grpc\n      module: model\n      service: gateway\n      endpoint: grpc\n      audience: {from: assistant/model-audience}\n      scopes:\n        - resource_kind: modelservice.profiles\n          actions: [invoke]\n    - id: chat\n", 1)
	require.NotEqual(t, consumingSolution, model)

	authority, err := deriveShopAuthority(t, model)

	require.NoError(t, err)
	audiences := map[string]string{}
	for _, binding := range authority.Bindings {
		audiences[binding.ID] = binding.Audience
	}
	require.Equal(t, map[string]string{"documents": "documents", "model": "model-gateway"}, audiences)
}

// A solution whose manifest Load refuses on a section that has nothing to do
// with consumption, or that asks for no scopes, renders as a principal asking
// for nothing.
func TestAnUnscopedSolutionAsksForNothing(t *testing.T) {
	unscoped := strings.Replace(consumingSolution,
		"      audience: {from: documents/documents-prefix}\n      scopes:\n        - resource_kind: documents.passages\n          actions: [read]\n        - resource_kind: documents.collections\n          actions: [read]\n", "", 1)
	require.NotEqual(t, consumingSolution, unscoped)
	for name, solution := range map[string]string{
		"agentless.yaml":          consumptionFixture(t, "agentless.yaml"),
		"undeclared-section.yaml": consumptionFixture(t, "undeclared-section.yaml"),
		"no scopes":               unscoped,
	} {
		t.Run(name, func(t *testing.T) {
			authority, err := deriveShopAuthority(t, solution)
			require.NoError(t, err)
			require.Equal(t, &solutionhost.Authority{Principal: "shop"}, authority)
		})
	}
}

// A scoped entry for a module the composition does not compose is refused at
// render, where its author reads it.
func TestScopesAtAModuleNotComposedAreRefused(t *testing.T) {
	elsewhere := strings.Replace(consumingSolution, "      module: documents\n      service: documents\n", "      module: archive\n      service: documents\n", 1)
	require.NotEqual(t, consumingSolution, elsewhere)

	_, err := deriveShopAuthority(t, elsewhere)

	require.ErrorContains(t, err, "at module archive")
	require.ErrorContains(t, err, "does not compose")
}

// A solution asking for scopes at itself is refused by name — whether it names
// itself as the module or the composition resolves the audience to its own
// principal.
func TestScopesAtTheSolutionItselfAreRefused(t *testing.T) {
	t.Run("by module", func(t *testing.T) {
		self := strings.Replace(consumingSolution, "      module: documents\n      service: documents\n      endpoint: rest\n",
			"      module: shop\n      service: api\n      endpoint: http\n", 1)
		require.NotEqual(t, consumingSolution, self)
		_, err := deriveShopAuthority(t, self)
		require.ErrorContains(t, err, "asks for scopes at itself")
	})
	t.Run("by resolved audience", func(t *testing.T) {
		workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
		require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "documents.env"), []byte("DOCUMENTS_PREFIX=shop\n"), 0o644))
		env := selectedEnvironment(t, workspace, "staging")
		_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
		require.ErrorContains(t, err, `at audience "shop", which is its own principal`)
	})
}

// One request, from one declaration: a contract AND scoped consumption is
// refused by name — even a contract that declares only destinations.
func TestASolutionCannotAskForAuthorityTwice(t *testing.T) {
	for name, contract := range map[string]string{
		"a contract with bindings":     authorityContract,
		"a destinations-only contract": "schema: codefly/module-contract/v1\nprincipal: shop\nnamespaces: []\nqueues: []\nscope_ceilings: []\nbindings: []\ndestinations:\n  - id: api-http\n    service: api\n    endpoint: http\n    kind: module\n",
	} {
		t.Run(name, func(t *testing.T) {
			workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
			require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(contract), 0o644))
			env := selectedEnvironment(t, workspace, "staging")
			_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
			require.ErrorContains(t, err, "asks for authority from one declaration")
		})
	}
}

// What the authority read DOES read, it reads strictly: an invalid scope fails
// the render.
func TestAnInvalidConsumedScopeFailsTheRender(t *testing.T) {
	_, err := deriveShopAuthority(t, strings.Replace(consumingSolution, "actions: [read]\n        - resource_kind: documents.collections", "actions: [\"*\"]\n        - resource_kind: documents.collections", 1))
	require.ErrorContains(t, err, "the api.consumes an authority request is derived from")
}

// Scoped consumption with no module-identity service is a request nothing would
// run as: refused naming where it was asked, never dropped.
func TestScopedConsumptionWithoutAnIdentityIsRefused(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	services[0].ModuleIdentity = false

	instance, _, err := moduleInstanceOf(context.Background(), workspace, module, services, env, &InventoryPackage{ID: "acme/shop", Version: "1.2.0"}, nil, nil)

	require.Nil(t, instance)
	require.ErrorContains(t, err, "asks for scopes on api.consumes")
	require.ErrorContains(t, err, "module-identity")
}

func TestUnscopedSolutionPreservesModuleContract(t *testing.T) {
	_, module := writeConsumingSolutionWorkspace(t, consumptionFixture(t, "agentless.yaml"))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract), 0o644))
	request, err := authorityRequestOf(module)
	require.NoError(t, err)
	require.NotEmpty(t, request.contract.Bindings)
	require.Contains(t, request.source, modulecontract.FileName)
}

// A native run and a cell's render derive one request from one composition:
// the manifests they deliver carry the same authority.
func TestANativeRunAndARenderRequestTheSameAuthority(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	env := selectedEnvironment(t, workspace, "staging")
	_, deployed := renderShopManifest(t, workspace, module, env)
	native, err := (&NativeDeliverer{Workspace: workspace, Env: env, StateDir: t.TempDir()}).Render(t.Context(), &NativeWorkload{
		Module:   module,
		Services: loadServices(t, workspace, "shop", "api"),
		Started:  []NativeIdentity{{Module: "shop", Service: "api", Account: "api", SPIFFEID: "spiffe://cluster.example/ns/acme-shop/sa/api"}},
	})
	require.NoError(t, err)
	require.Equal(t, deployed.Authority, native.Authority)
	require.Equal(t, deployed.Workloads, native.Workloads)
	require.Equal(t, deployed.Binding, native.Binding)
}
