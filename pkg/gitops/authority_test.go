package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/solutionhost/modulecontract"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// authorityContract is a module contract of the agreed shape, with every slot
// pointing at the composition's configuration.
// authorityContract is a contract the signed authority document carries
// whole: one namespace, no queue, no module ceiling, no destination, no
// binding key and no lookup method — see uncarriedContract for the rest.
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

// uncarriedContract declares everything core's authority document has no
// field for: a module ceiling, a destination, a second namespace, two queues,
// a binding key and a lookup method.
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

// writeAuthorityWorkspace lays down a workspace composing module "shop" whose
// service "api" declares module-identity, publishing a contract, with the
// configuration the contract's slots resolve from.
func writeAuthorityWorkspace(t *testing.T) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace := writeCellWorkspace(t)
	files := map[string]string{
		filepath.Join("modules", "shop", resources.ModuleConfigurationName):                     "kind: module\nname: shop\nservices:\n  - name: api\n",
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName): cellServiceYAML("api", "shop") + "module-identity: true\n",
		filepath.Join("modules", "shop", modulecontract.FileName):                               authorityContract,
		filepath.Join("modules", "shop", "module.package.codefly.yaml"):                         "schema: codefly/module-package/v1\nid: acme/shop\nversion: 1.2.0\n",
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

func readDeliveredAuthority(t *testing.T, destination, environment, file string) (*solutionhost.AuthorityDocument, solutionAuthorityConfigMap) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionAuthorityOverlay(environment)), file))
	require.NoError(t, err)
	var carrier solutionAuthorityConfigMap
	require.NoError(t, yaml.Unmarshal(data, &carrier))
	document, err := solutionhost.ParseAuthority([]byte(carrier.Data[solutionhost.AuthorityFileName]))
	require.NoError(t, err)
	return document, carrier
}

// TestRenderDerivesAuthorityFromTheModuleContract pins the derivation: one
// document for the module-identity service, named after the binding it is
// granted over, approving the build its unit runs, granting the contract's
// principal one unit of authority per binding and operation, with every slot
// resolved from the composition.
func TestRenderDerivesAuthorityFromTheModuleContract(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "shop-api@example.iam.test"}}

	instances, undeclared, err := authorityInstancesOf(ctx, workspace, module, services, env, units)
	require.NoError(t, err)
	require.Empty(t, undeclared)
	require.Len(t, instances, 1)
	require.Equal(t, "api", instances[0].Service)

	destination := moduleRenderDestination(workspace, "shop")
	result, err := RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination, Module: "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop", Workspace: "acme", Host: env.Host,
		Units:   promotableServiceGraph("shop", []string{"api"}),
		Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
		SolutionInstances: []SolutionInstance{{
			Kind: solutionhost.KindModule, Name: "shop", Package: "acme/shop", Version: "1.2.0",
			ReleaseDigest: testReleaseDigest, Units: units,
		}},
		AuthorityInstances: instances,
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(cellDeployment), 0o644)
	})
	require.NoError(t, err)
	require.Equal(t, solutionAuthorityDir, result.Inventory.SolutionAuthorityPath)
	require.Len(t, result.SolutionAuthorities, 1)
	declared := result.SolutionAuthorities[0]
	require.Equal(t, "acme.staging.shop-authority", declared.Authority)
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), declared.Build, "the approved build is the authenticating container's image, never the sidecar's")

	document, carrier := readDeliveredAuthority(t, destination, "staging", "acme.staging.shop-authority.yaml")
	require.Equal(t, solutionhost.SchemaAuthorityV1, document.Schema)
	require.Equal(t, "acme.staging.shop-authority", document.Authority)
	require.Equal(t, "acme.staging.shop", document.PresenceBinding, "granted over exactly this instance's presence binding")
	require.Equal(t, uint64(1), document.Generation)
	require.Equal(t, uint64(1), document.EffectiveFrom)
	require.Equal(t, "acme", document.OwnershipDomain)
	require.Equal(t, uint64(1), document.EnvelopeRevision)
	require.Equal(t, solutionhost.HostTarget{Coordinate: "example/staging/region-a", Component: "platform-host"}, document.Host)
	require.Equal(t, solutionhost.ImageDigest("sha256:"+strings.Repeat("a", 64)), document.ApprovedBuild)
	require.Len(t, document.Principals, 1)
	require.Equal(t, "shop", document.Principals[0].Principal, "the principal is the module's own name")
	require.Equal(t, []solutionhost.AuthorityBinding{
		// Unit IDs are scoped by the presence binding, so two instances of one
		// module on one host hold distinct units.
		{ID: "acme.staging.shop:annotations:headless", Revision: 2, Audience: "annotations", Scope: "annotations.annotations:redact,annotations.vocabularies:write", Namespace: "shop"},
		{ID: "acme.staging.shop:model:invoke", Revision: 1, Audience: "model-gateway", Scope: "modelservice.profiles:invoke,modelservice.profiles:read", Namespace: "shop"},
		{ID: "acme.staging.shop:model:lookup", Revision: 1, Audience: "model-gateway", Scope: "modelservice.profiles:read", Namespace: "shop"},
	}, document.Principals[0].Bindings)
	// Delivered to the authority namespace, labelled for the host, under the
	// document's own data key; the carrier arrives at publish.
	require.Equal(t, authorityNamespace, carrier.Metadata.Namespace)
	require.Equal(t, "solution-authority-acme.staging.shop-authority", carrier.Metadata.Name)
	require.Equal(t, "shop", carrier.Metadata.Labels[solutionLabel])
	require.Equal(t, "acme.staging.shop", carrier.Metadata.Labels[bindingLabel], "selected by the binding it is granted over, as the presence document is")
	require.NotContains(t, carrier.Data, authorityCarrierKey)
	kustomization, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionAuthorityOverlay("staging")), kustomizationFile))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), "acme.staging.shop-authority.yaml")
}

func TestAuthorityIsNotDerivedWithoutAContractOrAnIdentity(t *testing.T) {
	ctx := context.Background()
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	services := loadServices(t, workspace, "shop", "api")
	instances, undeclared, err := authorityInstancesOf(ctx, workspace, module, services, env, nil)
	require.NoError(t, err)
	require.Nil(t, instances)
	require.Contains(t, undeclared, modulecontract.FileName)

	// A contract with no module-identity service is a request nothing would
	// present: refused, never dropped.
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract), 0o644))
	instances, _, err = authorityInstancesOf(ctx, workspace, module, services, env, nil)
	require.Error(t, err)
	require.Nil(t, instances)
	require.Contains(t, err.Error(), "module-identity")
}

// TestAModuleWithAContractMustDeclarePresence: the render declares the
// module's presence before its authority, and a module with no resolved
// package declares none — the path that used to return before authority
// derivation ran, so a parseable contract was dropped silently and, over a
// delivered declaration, read as a withdrawal. With a contract on disk the
// render refuses instead; without one, the absence is recorded and the render
// goes on.
func TestAModuleWithAContractMustDeclarePresence(t *testing.T) {
	ctx := context.Background()
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	services := loadServices(t, workspace, "shop", "api")

	render := &moduleRender{workspace: workspace, module: module, env: env, options: &RenderOptions{}}
	require.NoError(t, render.declareInstances(ctx, services))
	require.Empty(t, render.options.SolutionInstances)
	require.Contains(t, render.options.UndeclaredAuthority, "declares no presence")

	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract), 0o644))
	render = &moduleRender{workspace: workspace, module: module, env: env, options: &RenderOptions{}}
	err = render.declareInstances(ctx, services)
	require.Error(t, err)
	require.Contains(t, err.Error(), "publishes "+modulecontract.FileName+" but declares no presence")
	require.Empty(t, render.options.SolutionInstances)
}

// TestAuthorityIsPresentedByOneService: two services each declaring
// module-identity would be two authority documents over one binding — two
// builds under one principal — of which a host, holding one authority record
// per binding, could activate at most one. The render refuses the pair and
// names both, rather than letting the host refuse one of them at apply.
func TestAuthorityIsPresentedByOneService(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	worker := filepath.Join(module.Dir(), "services", "worker", resources.ServiceConfigurationName)
	require.NoError(t, os.MkdirAll(filepath.Dir(worker), 0o755))
	require.NoError(t, os.WriteFile(worker, []byte(cellServiceYAML("worker", "shop")+"module-identity: true\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), resources.ModuleConfigurationName), []byte("kind: module\nname: shop\nservices:\n  - name: api\n  - name: worker\n"), 0o644))
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	services := loadServices(t, workspace, "shop", "api", "worker")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api"}, {Name: "worker", Path: "services/worker"}}

	_, _, err = authorityInstancesOf(ctx, workspace, module, services, env, units)
	require.Error(t, err)
	require.Contains(t, err.Error(), "module-identity on the services api, worker")
	require.Contains(t, err.Error(), "one authority record per binding")
}

// TestAuthorityRefusesAPrincipalThatIsNotTheModule: the contract is written
// in the module's repository, so a principal it names is self-asserted; a
// module claiming another's principal would claim that principal's bindings.
// The principal is the module's own name, and anything else is refused.
func TestAuthorityRefusesAPrincipalThatIsNotTheModule(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(strings.Replace(authorityContract, "principal: shop", "principal: billing", 1)), 0o644))
	services := loadServices(t, workspace, "shop", "api")
	_, _, err := authorityInstancesOf(ctx, workspace, module, services, env, []SolutionArtifactUnit{{Name: "api", Path: "services/api"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), `principal is "billing"`)
	require.Contains(t, err.Error(), "the contract declares principal: shop")
}

// TestAuthorityRefusesWhatTheDocumentCannotCarry: a declaration the signed
// authority document has no field for is refused, by name, rather than dropped
// between the contract and the document — a host cannot enforce what it never
// receives, and a changed declaration that leaves the document unchanged is
// enforced as before. Each names the field core's AuthorityBinding would need.
func TestAuthorityRefusesWhatTheDocumentCannotCarry(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"),
		[]byte("MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\nMODEL_BINDING=model-binding\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(uncarriedContract), 0o644))
	services := loadServices(t, workspace, "shop", "api")
	_, _, err := authorityInstancesOf(ctx, workspace, module, services, env, []SolutionArtifactUnit{{Name: "api", Path: "services/api"}})
	require.Error(t, err)
	for _, want := range []string{
		"2 queues", "2 namespaces", "scope_ceilings", "destinations",
		"binding model binding_key", "binding model lookup.method",
		"needs core's solutionhost.AuthorityBinding to grow those fields",
	} {
		require.Contains(t, err.Error(), want)
	}
}

func TestAuthorityRefusesAnUnresolvedSlot(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"), []byte("MODEL_AUDIENCE=model-gateway\n"), 0o644))
	services := loadServices(t, workspace, "shop", "api")
	_, _, err := authorityInstancesOf(ctx, workspace, module, services, env, []SolutionArtifactUnit{{Name: "api", Path: "services/api"}})
	require.ErrorIs(t, err, modulecontract.ErrUnresolvedSlot)
	require.Contains(t, err.Error(), "assistant/model-resource-kind")
}

// TestAuthorityReachesArgoUnderItsOwnProject: the authority overlay is its own
// Argo component, applied into the authority namespace under an AppProject of
// its own that admits that one destination and two kinds — the carrier
// ConfigMap and the delivery Job. The module's project never names the
// authority namespace: a project destination applies to every Application in
// the project, so it would let any unit overlay run a pod there as the
// platform's delivery account. "Signed AND isolated" is this test.
func TestAuthorityReachesArgoUnderItsOwnProject(t *testing.T) {
	root := t.TempDir()
	targetPath := "environments/deployments/modules/shop"
	inventory := &Inventory{
		SchemaVersion: SchemaVersion, Module: "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Units:                 []InventoryUnit{{Kind: UnitKindService, Module: "shop", Name: "api", Path: "services/api"}},
		SolutionAuthorityPath: solutionAuthorityDir,
	}
	writeOverlay(t, filepath.Join(root, "services", "api", "overlays", "staging"))
	writeOverlay(t, filepath.Join(root, solutionAuthorityDir, "overlays", "staging"))
	config := &repositoryConfig{RepoURL: "https://github.com/example/manifests.git"}
	require.NoError(t, generateArgoBootstrap(context.Background(), config, root, targetPath, inventory, "staging", strings.Repeat("c", 40), ""))

	set, err := os.ReadFile(filepath.Join(root, "bootstrap", "applicationset.yaml"))
	require.NoError(t, err)
	// The generated set passes the validation publication runs over it: the
	// validator reads the fields the Application template consumes.
	var generated map[string]any
	require.NoError(t, yaml.Unmarshal(set, &generated))
	spec, _ := generated["spec"].(map[string]any)
	require.NoError(t, validateComponentProjects(spec, "acme-staging"))
	var applicationSet struct {
		Spec struct {
			Generators []struct {
				Matrix struct {
					Generators []struct {
						List *struct {
							Elements []argoComponentElement `yaml:"elements"`
						} `yaml:"list"`
					} `yaml:"generators"`
				} `yaml:"matrix"`
			} `yaml:"generators"`
			Template struct {
				Spec struct {
					Project     string          `yaml:"project"`
					Destination argoDestination `yaml:"destination"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(set, &applicationSet))
	require.Equal(t, "{{ .project }}", applicationSet.Spec.Template.Spec.Project)
	require.Equal(t, "{{ .namespace }}", applicationSet.Spec.Template.Spec.Destination.Namespace)
	byComponent := map[string]argoComponentElement{}
	for _, generator := range applicationSet.Spec.Generators {
		for _, leaf := range generator.Matrix.Generators {
			if leaf.List == nil {
				continue
			}
			for _, element := range leaf.List.Elements {
				byComponent[element.Component] = element
			}
		}
	}
	authority := byComponent["shop-solution-authority"]
	require.Equal(t, targetPath+"/"+solutionAuthorityDir+"/overlays/staging", authority.Overlay)
	require.Equal(t, "acme-staging-authority", authority.Project)
	require.Equal(t, authorityNamespace, authority.Namespace)
	unit := byComponent["shop-api"]
	require.Equal(t, "acme-staging", unit.Project)
	require.Equal(t, "acme-shop", unit.Namespace)

	project, err := os.ReadFile(filepath.Join(root, "bootstrap", "project.yaml"))
	require.NoError(t, err)
	require.NotContains(t, string(project), authorityNamespace, "the module's project never reaches the authority namespace")
	require.Contains(t, string(project), "namespace: acme-shop")
	isolated, err := os.ReadFile(filepath.Join(root, "bootstrap", authorityProjectFile))
	require.NoError(t, err)
	var authorityProject argoProjectManifest
	require.NoError(t, yaml.Unmarshal(isolated, &authorityProject))
	require.Equal(t, "acme-staging-authority", authorityProject.Metadata.Name)
	require.Equal(t, []argoDestination{{Namespace: authorityNamespace, Server: inClusterServer}}, authorityProject.Spec.Destinations)
	require.Empty(t, authorityProject.Spec.ClusterResourceWhitelist)
	require.Equal(t, []argoResourceAuthority{{Group: "", Kind: kindConfigMap}, {Group: "batch", Kind: kindJob}}, authorityProject.Spec.NamespaceResourceWhitelist,
		"a Deployment, a CronJob or a Secret in the authority overlay is refused at apply")
	kustomization, err := os.ReadFile(filepath.Join(root, "bootstrap", "kustomization.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), authorityProjectFile)
	require.NoError(t, validateBootstrapUnits(filepath.Join(root, "bootstrap"), targetPath, inventory, "staging"))
}

// TestComponentProjectsBindPathProjectAndNamespace: the authority overlay is
// delivered under the authority project only, and the authority project
// delivers nothing but the authority overlay, into its namespace — the three
// name each other, so no component can borrow the project for another path.
func TestComponentProjectsBindPathProjectAndNamespace(t *testing.T) {
	spec := func(path, project, namespace string) map[string]any {
		return map[string]any{"generators": []any{map[string]any{"list": map[string]any{"elements": []any{
			map[string]any{"component": "c", "overlay": path, "project": project, "namespace": namespace},
		}}}}}
	}
	authority := argoAuthorityProjectName("shop")
	require.NoError(t, validateComponentProjects(spec("deployments/modules/shop/solution-authority/overlays/prod", authority, authorityNamespace), "shop"))
	require.NoError(t, validateComponentProjects(spec("deployments/modules/shop/services/api/overlays/prod", "shop", "shop"), "shop"))
	err := validateComponentProjects(spec("deployments/modules/shop/solution-authority/overlays/prod", "shop", "shop"), "shop")
	require.Error(t, err)
	require.Contains(t, err.Error(), "authority overlay")
	err = validateComponentProjects(spec("deployments/modules/shop/services/api/overlays/prod", authority, authorityNamespace), "shop")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not the authority overlay")
	// Every field the template consumes is required; another generator's
	// elements (the tenant matrix) carry none of them and are not components.
	err = validateComponentProjects(spec("deployments/modules/shop/services/api/overlays/prod", "", "shop"), "shop")
	require.Error(t, err)
	require.Contains(t, err.Error(), "lacks a component, overlay, project or namespace")
	tenants := map[string]any{"generators": []any{map[string]any{"list": map[string]any{"elements": []any{map[string]any{"tenant": "acme", "server": "https://kubernetes.default.svc"}}}}}}
	require.NoError(t, validateComponentProjects(tenants, "shop"))
	// A component element is one that names a component field: a missing
	// key and a mistyped value are both refused, and an element whose four
	// fields are all malformed is refused rather than mistaken for the
	// tenant matrix.
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
