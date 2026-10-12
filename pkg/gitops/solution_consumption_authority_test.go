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

func deriveShopAuthority(t *testing.T, solution string) ([]AuthorityInstance, string, error) {
	t.Helper()
	workspace, module := writeConsumingSolutionWorkspace(t, solution)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "shop-api@example.iam.test"}}
	return authorityInstancesOf(context.Background(), workspace, module, services, env, units, nil)
}

// THE DELIVERY HALF OF A SOLUTION'S CEILING: its declared consumption becomes
// the signed authority document's invoke unit for the module it calls on a
// person's behalf — its audience resolved from the composition, bounded by
// exactly the declared scopes — and the consumptions that ask for no scopes
// become nothing.
func TestRenderDerivesAuthorityFromASolutionsDeclaredConsumption(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "shop-api@example.iam.test"}}

	instances, undeclared, err := authorityInstancesOf(context.Background(), workspace, module, services, env, units, nil)
	require.NoError(t, err)
	require.Empty(t, undeclared)
	require.Len(t, instances, 1)

	destination, _ := renderShopAuthority(t, workspace, env, units, instances)
	document, _ := readDeliveredAuthority(t, destination, "staging", "acme.staging.shop-authority.yaml")
	require.Len(t, document.Principals, 1)
	require.Equal(t, "shop", document.Principals[0].Principal, "a composed solution's principal is the name it is composed under")
	require.Equal(t, []solutionhost.AuthorityBinding{
		{ID: "acme.staging.shop:documents:invoke", Revision: 1, Audience: "documents", Scope: "documents.collections:read,documents.passages:read"},
	}, document.Principals[0].Bindings, "one invoke unit, for the one consumption that asks for scopes")
	require.Empty(t, document.Queues)
	require.Empty(t, document.Namespaces)
	require.Empty(t, document.ScopeCeilings)
}

// The scoped consumer fixture renders one documents binding end to end, and
// deterministically: a second render is byte-identical.
func TestAScopedConsumerRendersDeterministically(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumptionFixture(t, "scoped-consumer.yaml"))
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "shop-api@example.iam.test"}}
	instances, _, err := authorityInstancesOf(context.Background(), workspace, module, services, env, units, nil)
	require.NoError(t, err)

	destination, _ := renderShopAuthority(t, workspace, env, units, instances)
	document, _ := readDeliveredAuthority(t, destination, "staging", "acme.staging.shop-authority.yaml")
	require.Equal(t, []solutionhost.AuthorityBinding{
		{ID: "acme.staging.shop:documents:invoke", Revision: 1, Audience: "documents", Scope: "documents:read"},
	}, document.Principals[0].Bindings)

	path := filepath.Join(filepath.FromSlash(solutionAuthorityOverlay("staging")), "acme.staging.shop-authority.yaml")
	first, err := os.ReadFile(filepath.Join(destination, path))
	require.NoError(t, err)
	again, _ := renderShopAuthority(t, workspace, env, units, instances)
	second, err := os.ReadFile(filepath.Join(again, path))
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

// The audience follows the composition, as a contract's does: the composition
// says the model module verifies model-gateway, and that is the audience.
func TestTheConsumptionAudienceFollowsTheComposition(t *testing.T) {
	model := strings.Replace(consumingSolution, "    - id: chat\n",
		"    - id: model\n      protocol: grpc\n      module: model\n      service: gateway\n      endpoint: grpc\n      audience: {from: assistant/model-audience}\n      scopes:\n        - resource_kind: modelservice.profiles\n          actions: [invoke]\n    - id: chat\n", 1)
	require.NotEqual(t, consumingSolution, model)

	instances, _, err := deriveShopAuthority(t, model)

	require.NoError(t, err)
	audiences := map[string]string{}
	for _, binding := range instances[0].Contract.Bindings {
		audiences[binding.ID] = binding.Audience
	}
	require.Equal(t, map[string]string{"documents": "documents", "model": "model-gateway"}, audiences)
}

// BLAST RADIUS: a solution whose manifest Load refuses on a section that has
// nothing to do with consumption, and which asks for no authority, renders as
// before.
func TestAnUnscopedSolutionStillRenders(t *testing.T) {
	for _, name := range []string{"agentless.yaml", "undeclared-section.yaml"} {
		t.Run(name, func(t *testing.T) {
			instances, undeclared, err := deriveShopAuthority(t, consumptionFixture(t, name))
			require.NoError(t, err, "a solution that asks for no scopes asks for no authority and renders as before")
			require.Nil(t, instances)
			require.Contains(t, undeclared, "asks for no scopes on api.consumes")
		})
	}
}

// A solution that consumes modules and asks for no scopes asks for no
// authority, and the render says why.
func TestASolutionAskingForNoScopesAsksForNoAuthority(t *testing.T) {
	unscoped := strings.Replace(consumingSolution,
		"      audience: {from: documents/documents-prefix}\n      scopes:\n        - resource_kind: documents.passages\n          actions: [read]\n        - resource_kind: documents.collections\n          actions: [read]\n", "", 1)
	require.NotEqual(t, consumingSolution, unscoped)

	instances, undeclared, err := deriveShopAuthority(t, unscoped)

	require.NoError(t, err)
	require.Nil(t, instances)
	require.Contains(t, undeclared, "asks for no scopes on api.consumes")
}

// A scoped entry for a module the composition does not compose is refused at
// render, where its author reads it.
func TestScopesAtAModuleNotComposedAreRefused(t *testing.T) {
	elsewhere := strings.Replace(consumingSolution, "      module: documents\n      service: documents\n", "      module: archive\n      service: documents\n", 1)
	require.NotEqual(t, consumingSolution, elsewhere)

	_, _, err := deriveShopAuthority(t, elsewhere)

	require.ErrorContains(t, err, "at module archive")
	require.ErrorContains(t, err, "does not compose")
}

// A solution asking for scopes at itself is refused by name — whether it names
// itself as the module or the composition resolves the audience to its own
// principal — rather than rendered for the host to omit with a logged reason.
func TestScopesAtTheSolutionItselfAreRefused(t *testing.T) {
	t.Run("by module", func(t *testing.T) {
		self := strings.Replace(consumingSolution, "      module: documents\n      service: documents\n      endpoint: rest\n",
			"      module: shop\n      service: api\n      endpoint: http\n", 1)
		require.NotEqual(t, consumingSolution, self)
		_, _, err := deriveShopAuthority(t, self)
		require.ErrorContains(t, err, "asks for scopes at itself")
	})
	t.Run("by resolved audience", func(t *testing.T) {
		workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
		require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "documents.env"), []byte("DOCUMENTS_PREFIX=shop\n"), 0o644))
		env := selectedEnvironment(t, workspace, "staging")
		services := loadServices(t, workspace, "shop", "api")
		_, _, err := authorityInstancesOf(context.Background(), workspace, module, services, env, nil, nil)
		require.ErrorContains(t, err, `at audience "shop", which is its own principal`)
	})
}

// One request, from one declaration: a contract AND scoped consumption is
// refused by name — even a contract that declares only destinations and no
// bindings. Merging the two is a decision nothing has made yet.
func TestASolutionCannotAskForAuthorityTwice(t *testing.T) {
	for name, contract := range map[string]string{
		"a contract with bindings":     authorityContract,
		"a destinations-only contract": "schema: codefly/module-contract/v1\nprincipal: shop\nnamespaces: []\nqueues: []\nscope_ceilings: []\nbindings: []\ndestinations:\n  - id: api-http\n    service: api\n    endpoint: http\n    kind: module\n",
	} {
		t.Run(name, func(t *testing.T) {
			workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
			require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(contract), 0o644))
			env := selectedEnvironment(t, workspace, "staging")
			services := loadServices(t, workspace, "shop", "api")

			_, _, err := authorityInstancesOf(context.Background(), workspace, module, services, env, nil, nil)

			require.ErrorContains(t, err, "asks for authority from one declaration")
		})
	}
}

// What the authority read DOES read, it reads strictly: an invalid scope fails
// the render.
func TestAnInvalidConsumedScopeFailsTheRender(t *testing.T) {
	_, _, err := deriveShopAuthority(t, strings.Replace(consumingSolution, "actions: [read]\n        - resource_kind: documents.collections", "actions: [\"*\"]\n        - resource_kind: documents.collections", 1))
	require.ErrorContains(t, err, "the api.consumes an authority document is derived from")
}

// Scoped consumption with no module-identity service is a request nothing would
// present: refused naming where it was asked, never dropped.
func TestScopedConsumptionWithoutAnIdentityIsRefused(t *testing.T) {
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	service := filepath.Join(workspace.Dir(), "modules", "shop", "services", "api", resources.ServiceConfigurationName)
	data, err := os.ReadFile(service)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(service, []byte(strings.Replace(string(data), "module-identity: true\n", "", 1)), 0o644))
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	instances, _, err := authorityInstancesOf(context.Background(), workspace, module, services, env, nil, nil)

	require.Nil(t, instances)
	require.ErrorContains(t, err, "asks for scopes on api.consumes")
	require.ErrorContains(t, err, "module-identity")
}

// Two solutions in one composition: each is its own principal, and every unit
// is scoped by its own presence binding, so two solutions consuming the same
// module never share a unit ID.
func TestTwoSolutionsInOneComposition(t *testing.T) {
	m, err := solutionmanifest.Load([]byte(consumingSolution))
	require.NoError(t, err)
	resolve := func(principal string) *modulecontract.Resolved {
		contract, err := m.ConsumptionContract(principal)
		require.NoError(t, err)
		resolved, err := contract.Resolve(modulecontract.MapValues{Public: map[string]map[string]string{"documents": {"DOCUMENTS_PREFIX": "documents"}}})
		require.NoError(t, err)
		return resolved
	}
	a := authorityBindings("acme.staging.first", resolve("first"))
	b := authorityBindings("acme.staging.second", resolve("second"))
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	require.NotEqual(t, a[0].ID, b[0].ID)
	require.Equal(t, a[0].Audience, b[0].Audience)
}
