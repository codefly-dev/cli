package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// authorityContract is a module contract of the agreed shape, with every slot
// pointing at the composition's configuration: one namespace, no queue, no
// module ceiling, no destination — see uncarriedContract for the rest.
const authorityContract = `schema: codefly/module-contract/v1
principal: shop
namespaces: [shop]
queues: []
scope_ceilings: []
bindings:
  - id: model
    operations: [invoke, lookup]
    audience: {from: assistant/model-audience}
    resource_kind: {from: assistant/model-resource-kind}
    scope_ceiling:
      invoke: [invoke, read]
      lookup: [read]
  - id: annotations
    revision: 2
    operations: [headless]
    audience: {from: assistant/annotations-prefix}
    scope_ceiling:
      headless:
        - resource_kind: annotations.vocabularies
          actions: [write]
        - resource_kind: annotations.annotations
          actions: [redact]
destinations: []
`

// uncarriedContract declares a module ceiling, a second namespace, two
// queues, a binding key and a lookup method, and a destination, which the
// manifest never carries and the render holds to the module's endpoints.
const uncarriedContract = `schema: codefly/module-contract/v1
principal: shop
namespaces: [shop, shop-audit]
queues: [shop.default, shop.bulk]
scope_ceilings:
  - resource_kind: shop.tasks
    actions: [execute]
bindings:
  - id: model
    operations: [invoke, lookup]
    audience: {from: assistant/model-audience}
    resource_kind: {from: assistant/model-resource-kind}
    binding_key: {from: assistant/model-binding}
    lookup: {method: header}
    scope_ceiling:
      invoke: [invoke]
      lookup: [read]
destinations:
  - id: chat-http
    service: api
    endpoint: http
    kind: module
`

// shopPackageManifest is the release module "shop" names, in the one shape a
// reader accepts: a module manifest quotes its id and version.
const shopPackageManifest = `kind: module-package
schema: codefly/module-package/v2
id: acme/shop
version: 1.2.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - services
contracts:
  composition: ">=1.0"
`

// writeAuthorityWorkspace lays down a workspace composing module "shop" whose
// service "api" declares module-identity, publishing a contract, with the
// configuration the contract's slots resolve from.
func writeAuthorityWorkspace(t *testing.T) (*resources.Workspace, *resources.Module) {
	t.Helper()
	return writeAuthorityWorkspaceRunning(t, exposedHostedServiceYAML("api", "shop")+"module-identity: true\n")
}

// writeAuthorityWorkspaceRunning is writeAuthorityWorkspace with the service
// declaration supplied, so a case about which agent runs "api" states that
// rather than re-laying the whole composition beside it.
func writeAuthorityWorkspaceRunning(t *testing.T, service string) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace := writeHostedWorkspace(t)
	files := map[string]string{
		filepath.Join("modules", "shop", resources.ModuleConfigurationName):                     "kind: module\nname: shop\nservices:\n  - name: api\n",
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName): service,
		filepath.Join("modules", "shop", modulecontract.FileName):                               authorityContract,
		filepath.Join("modules", "shop", "module.package.codefly.yaml"):                         shopPackageManifest,
		filepath.Join("configurations", "staging", "assistant.env"):                             "MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nANNOTATIONS_PREFIX=annotations\n",
	}
	for rel, content := range files {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

// shopAuthority is the authority block the shop module's manifest carries,
// as the render derives it.
func shopAuthority(t *testing.T, workspace *resources.Workspace, module *resources.Module, services []*resources.Service, env *environments.Environment) (*solutionhost.Authority, error) {
	t.Helper()
	request, err := authorityRequestOf(module)
	if err != nil {
		return nil, err
	}
	return moduleAuthority(context.Background(), workspace, module, services, env, request)
}

// renderShopManifest renders the shop module's owned tree the way a module
// render declares it (declareInstances), its api unit running hostedDeployment,
// and returns where it landed and the manifest it delivered.
func renderShopManifest(t *testing.T, workspace *resources.Workspace, module *resources.Module, env *environments.Environment) (string, *solutionhost.ModuleManifest) {
	t.Helper()
	ctx := context.Background()
	destination := moduleRenderDestination(workspace, "shop")
	render := &moduleRender{workspace: workspace, module: module, env: env, options: &RenderOptions{
		Destination: destination, Module: "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop", Workspace: "acme", Host: env.Host,
		Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
		Units:   promotableServiceGraph("shop", []string{"api"}),
	}}
	require.NoError(t, render.declareInstances(ctx, loadServices(t, workspace, "shop", "api")))
	require.NotNil(t, render.options.Instance)
	_, err := RenderOwnedTree(ctx, render.options, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(hostedDeployment), 0o644)
	})
	require.NoError(t, err)
	return destination, deliveredModuleManifestIn(t, destination, "staging", "acme.staging.shop")
}

// TestRenderDerivesAuthorityFromTheModuleContract pins the derivation: the
// module's own name as principal, one requested binding per contract binding
// with each operation's scopes, every slot resolved from the composition —
// and the manifest the render writes carries it.
func TestRenderDerivesAuthorityFromTheModuleContract(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	authority, err := shopAuthority(t, workspace, module, services, env)
	require.NoError(t, err)
	want := &solutionhost.Authority{
		Principal: "shop",
		Bindings: []solutionhost.AuthorityBinding{
			{ID: "model", Audience: "model-gateway", Operations: map[string][]string{
				"invoke": {"modelservice.profiles:invoke", "modelservice.profiles:read"},
				"lookup": {"modelservice.profiles:read"},
			}},
			{ID: "annotations", Audience: "annotations", Operations: map[string][]string{
				"headless": {"annotations.annotations:redact", "annotations.vocabularies:write"},
			}},
		},
		Namespaces: []string{"shop"},
	}
	require.Equal(t, want, authority)

	_, manifest := renderShopManifest(t, workspace, module, env)
	require.Equal(t, want, manifest.Authority)

	require.Equal(t, []solutionhost.Workload{{Service: "api", Container: "api"}}, manifest.Workloads)
}

// TestAModuleWithoutAContractAsksForNothingButRunsAsItsPrincipal: a module
// publishing no contract asks for no binding, and its manifest still names
// its principal, so every workload of it runs as it.
func TestAModuleWithoutAContractAsksForNothingButRunsAsItsPrincipal(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), modulecontract.FileName)))
	env := selectedEnvironment(t, workspace, "staging")
	authority, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.NoError(t, err)
	require.Equal(t, &solutionhost.Authority{Principal: "shop"}, authority)
}

// TestNoIdentityServiceDeclaresNoManifest: a module with no service declaring
// module-identity runs as nothing and declares no manifest, with the reason;
// with a contract, the request nothing would present is refused, never dropped.
func TestNoIdentityServiceDeclaresNoManifest(t *testing.T) {
	ctx := context.Background()
	workspace := writeHostedWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	services := loadServices(t, workspace, "shop", "api")
	pkg := &InventoryPackage{ID: "acme/shop", Version: "1.2.0"}
	instance, undeclared, err := moduleInstanceOf(ctx, workspace, module, services, env, pkg, nil, nil)
	require.NoError(t, err)
	require.Nil(t, instance)
	require.Contains(t, undeclared, "declares no module-identity service")

	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract), 0o644))
	instance, _, err = moduleInstanceOf(ctx, workspace, module, services, env, pkg, nil, nil)
	require.Error(t, err)
	require.Nil(t, instance)
	require.Contains(t, err.Error(), "module-identity")
}

// TestAModuleWithNoPackageDeclaresNoManifest: no package manifest names no
// release, so the module declares no manifest, and says why.
func TestAModuleWithNoPackageDeclaresNoManifest(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	// A module that asks for authority with no manifest to carry the request
	// is refused: the request would otherwise be dropped.
	render := &moduleRender{workspace: workspace, module: module, env: env, options: &RenderOptions{}}
	require.ErrorContains(t, render.declareInstances(ctx, services), "no package manifest")
	// One that asks for nothing declares no manifest, and says why.
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), modulecontract.FileName)))
	render = &moduleRender{workspace: workspace, module: module, env: env, options: &RenderOptions{}}
	require.NoError(t, render.declareInstances(ctx, services))
	require.Nil(t, render.options.Instance)
	require.Contains(t, render.options.UndeclaredManifest, "no package manifest")
}

// TestEveryIdentityServiceIsAWorkloadOfTheModule: two services each declaring
// module-identity are two workloads running as one principal: one instance
// lists both, each bound to the unit that renders it.
func TestEveryIdentityServiceIsAWorkloadOfTheModule(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	worker := filepath.Join(module.Dir(), "services", "worker", resources.ServiceConfigurationName)
	require.NoError(t, os.MkdirAll(filepath.Dir(worker), 0o755))
	require.NoError(t, os.WriteFile(worker, []byte(hostedServiceYAML("worker", "shop")+"module-identity: true\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), resources.ModuleConfigurationName), []byte("kind: module\nname: shop\nservices:\n  - name: api\n  - name: worker\n"), 0o644))
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	services := loadServices(t, workspace, "shop", "api", "worker")

	instance, _, err := moduleInstanceOf(ctx, workspace, module, services, env, &InventoryPackage{ID: "acme/shop", Version: "1.2.0"}, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []IdentityWorkload{{Service: "api"}, {Service: "worker"}}, instance.Identities)
	require.Equal(t, "shop", instance.Authority.Principal, "one principal, however many workloads run as it")
	require.NoError(t, bindIdentityUnits(instance, promotableServiceGraph("shop", []string{"api", "worker"})))
	require.Equal(t, []IdentityWorkload{{Service: "api", Path: "services/api"}, {Service: "worker", Path: "services/worker"}}, instance.Identities)

	missing, _, err := moduleInstanceOf(ctx, workspace, module, services, env, &InventoryPackage{ID: "acme/shop", Version: "1.2.0"}, nil, nil)
	require.NoError(t, err)
	err = bindIdentityUnits(missing, promotableServiceGraph("shop", []string{"api"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "service worker declares module-identity but this render delivers no unit for it")
}

// TestAuthorityRefusesAPrincipalThatIsNotTheModule: the contract is written
// in the module's repository, so a principal it names is self-asserted; a
// module claiming another's principal would claim that principal's grant.
func TestAuthorityRefusesAPrincipalThatIsNotTheModule(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(strings.Replace(authorityContract, "principal: shop", "principal: billing", 1)), 0o644))
	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.Error(t, err)
	require.Contains(t, err.Error(), `principal is "billing"`)
	require.Contains(t, err.Error(), "the contract declares principal: shop")
}

// TestAuthorityCarriesWhatTheContractDeclares: the module's own declarations
// — several queues, several namespaces, its scope ceilings' resource kinds —
// are requested whole; a binding's key and lookup method are the contract's,
// not the request's.
func TestAuthorityCarriesWhatTheContractDeclares(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"),
		[]byte("MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nMODEL_BINDING=model-binding\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(uncarriedContract), 0o644))
	authority, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.NoError(t, err)
	require.Equal(t, []string{"shop.default", "shop.bulk"}, authority.Queues)
	require.Equal(t, []string{"shop", "shop-audit"}, authority.Namespaces)
	require.Equal(t, []string{"shop.tasks"}, authority.ResourceKinds)
	require.Equal(t, []solutionhost.AuthorityBinding{{ID: "model", Audience: "model-gateway", Operations: map[string][]string{
		"invoke": {"modelservice.profiles:invoke"}, "lookup": {"modelservice.profiles:read"},
	}}}, authority.Bindings)
}

// TestTheRenderHoldsDestinationsToTheEndpointsTheModuleDeclares: a
// destination naming an endpoint no service of the module declares is a
// route nothing can take, refused by name. The api service declares grpc and
// http: a destination on http resolves; one on admin is refused.
func TestTheRenderHoldsDestinationsToTheEndpointsTheModuleDeclares(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"),
		[]byte("MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nMODEL_BINDING=model-binding\n"), 0o644))
	services := loadServices(t, workspace, "shop", "api")
	declare := func(contract string) (*moduleRender, error) {
		require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(contract), 0o644))
		render := &moduleRender{workspace: workspace, module: module, env: env, options: &RenderOptions{
			Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
			Units:   promotableServiceGraph("shop", []string{"api"}),
		}}
		return render, render.declareInstances(ctx, services)
	}

	render, err := declare(uncarriedContract)
	require.NoError(t, err)
	require.NotNil(t, render.options.Instance)

	_, err = declare(strings.Replace(uncarriedContract, "endpoint: http", "endpoint: admin", 1))
	require.ErrorIs(t, err, errUnrenderedDestination)
	require.Contains(t, err.Error(), "chat-http (api/admin)")
}

func TestAuthorityRefusesAnUnresolvedSlot(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"), []byte("MODEL_AUDIENCE=model-gateway\n"), 0o644))
	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorIs(t, err, modulecontract.ErrUnresolvedSlot)
	require.Contains(t, err.Error(), "assistant/model-resource-kind")
}

// TestComponentProjectsBindToTheSelectedProject: every component a module's
// ApplicationSet stamps is under the selected project and names its
// component, overlay, project and namespace; the tenant matrix's elements
// name none and are not components.
func TestComponentProjectsBindToTheSelectedProject(t *testing.T) {
	spec := func(path, project, namespace string) map[string]any {
		return map[string]any{"generators": []any{map[string]any{"list": map[string]any{"elements": []any{
			map[string]any{"component": "c", "overlay": path, "project": project, "namespace": namespace},
		}}}}}
	}
	require.NoError(t, validateComponentProjects(spec("deployments/modules/shop/services/api/overlays/prod", "shop", "shop"), "shop"))
	err := validateComponentProjects(spec("deployments/modules/shop/module-manifests/overlays/prod", "shop-authority", "platform-authority"), "shop")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not the selected AppProject")
	err = validateComponentProjects(spec("deployments/modules/shop/services/api/overlays/prod", "", "shop"), "shop")
	require.Error(t, err)
	require.Contains(t, err.Error(), "lacks a component, overlay, project or namespace")
	tenants := map[string]any{"generators": []any{map[string]any{"list": map[string]any{"elements": []any{map[string]any{"tenant": "acme", "server": "https://kubernetes.default.svc"}}}}}}
	require.NoError(t, validateComponentProjects(tenants, "shop"))
	for name, element := range map[string]map[string]any{
		"project key missing": {"component": "api", "overlay": "deployments/modules/shop/services/api/overlays/prod", "namespace": "shop"},
		"project mistyped":    {"component": "api", "overlay": "deployments/modules/shop/services/api/overlays/prod", "namespace": "shop", "project": 7},
		"all malformed":       {"component": 1, "overlay": true, "project": nil, "namespace": []any{"shop"}},
	} {
		malformed := map[string]any{"generators": []any{map[string]any{"list": map[string]any{"elements": []any{element}}}}}
		err = validateComponentProjects(malformed, "shop")
		require.Error(t, err, name)
		require.Contains(t, err.Error(), "every stamped Application needs all four", name)
	}
}
