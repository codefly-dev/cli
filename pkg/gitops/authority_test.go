package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/modulecontract"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// authorityContract is a module contract of the agreed shape, with every slot
// pointing at the composition's configuration.
const authorityContract = `schema: codefly/module-contract/v1
principal: assistant
namespaces: [assistant]
queues: []
scope_ceilings:
  - resource_kind: assistant.tasks
    actions: [execute]
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
// document per module-identity service, approving the build its unit runs,
// granting the contract's principal one unit of authority per binding and
// operation, with every slot resolved from the composition.
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
	require.Equal(t, "acme.staging.shop:api", declared.Authority)
	require.Equal(t, "sha256:"+strings.Repeat("a", 64), declared.Build, "the approved build is the authenticating container's image, never the sidecar's")

	document, carrier := readDeliveredAuthority(t, destination, "staging", "acme.staging.shop-api.yaml")
	require.Equal(t, solutionhost.SchemaAuthorityV1, document.Schema)
	require.Equal(t, "acme.staging.shop:api", document.Authority)
	require.Equal(t, "acme.staging.shop", document.PresenceBinding, "granted over exactly this instance's presence binding")
	require.Equal(t, uint64(1), document.Generation)
	require.Equal(t, uint64(1), document.EffectiveFrom)
	require.Equal(t, "acme", document.OwnershipDomain)
	require.Equal(t, uint64(1), document.EnvelopeRevision)
	require.Equal(t, solutionhost.HostTarget{Coordinate: "example/staging/region-a", Component: "platform-host"}, document.Host)
	require.Equal(t, solutionhost.ImageDigest("sha256:"+strings.Repeat("a", 64)), document.ApprovedBuild)
	require.Len(t, document.Principals, 1)
	require.Equal(t, "assistant", document.Principals[0].Principal)
	require.Equal(t, []solutionhost.AuthorityBinding{
		{ID: "assistant:annotations:headless", Revision: 2, Audience: "annotations", Scope: "annotations.annotations:redact,annotations.vocabularies:write", Namespace: "assistant"},
		{ID: "assistant:model:invoke", Revision: 1, Audience: "model-gateway", Scope: "modelservice.profiles:invoke,modelservice.profiles:read", Namespace: "assistant"},
		{ID: "assistant:model:lookup", Revision: 1, Audience: "model-gateway", Scope: "modelservice.profiles:read", Namespace: "assistant"},
	}, document.Principals[0].Bindings)
	// Delivered to the authority namespace, labelled for the host, under the
	// document's own data key; the carrier arrives at publish.
	require.Equal(t, authorityNamespace, carrier.Metadata.Namespace)
	require.Equal(t, "solution-authority-acme.staging.shop-api", carrier.Metadata.Name)
	require.Equal(t, "shop", carrier.Metadata.Labels[solutionLabel])
	require.NotContains(t, carrier.Data, authorityCarrierKey)
	kustomization, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(solutionAuthorityOverlay("staging")), kustomizationFile))
	require.NoError(t, err)
	require.Contains(t, string(kustomization), "acme.staging.shop-api.yaml")
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

	// A contract with no module-identity service has nothing to present it.
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract), 0o644))
	instances, undeclared, err = authorityInstancesOf(ctx, workspace, module, services, env, nil)
	require.NoError(t, err)
	require.Nil(t, instances)
	require.Contains(t, undeclared, "module-identity")
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

// TestAuthorityReachesArgoInTheAuthorityNamespace: the authority overlay is its
// own Argo component and the AppProject admits the authority namespace as a
// destination — the one namespace outside the module's own it may write to.
func TestAuthorityReachesArgoInTheAuthorityNamespace(t *testing.T) {
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
	require.Contains(t, string(set), "overlay: "+targetPath+"/"+solutionAuthorityDir+"/overlays/staging")
	project, err := os.ReadFile(filepath.Join(root, "bootstrap", "project.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(project), "namespace: "+authorityNamespace)
	require.Contains(t, string(project), "namespace: acme-shop")
	require.NoError(t, validateBootstrapUnits(filepath.Join(root, "bootstrap"), targetPath, inventory, "staging"))
}
