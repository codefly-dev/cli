package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// cellServiceYAML is a service declaring endpoints with their ports and a
// workspace configuration group, the two things the cell file and the group
// digest read off a service.
func cellServiceYAML(name string, groups ...string) string {
	service := devServiceYAML(name) + `endpoints:
  - name: grpc
    api: grpc
    visibility: internal
    allow-modules: [billing]
  - name: http
    api: http
    visibility: public
spec:
  deployment:
    endpoint-ports:
      grpc: 9090
      http: 8080
`
	if len(groups) > 0 {
		service += "workspace-configuration-dependencies:\n"
		for _, group := range groups {
			service += "  - " + group + "\n"
		}
	}
	return service
}

// writeCellWorkspace lays down a workspace composing module "shop" with a
// service "api" that declares endpoints, ports and a configuration group, and
// an environment naming a host.
func writeCellWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: acme
layout: modules
modules:
  - name: shop
  - name: billing
environments:
  - name: staging
    namespace: acme
    ingress:
      - name: shop-http
        service: shop/api
        endpoint: http
        hosts: [shop.example.test]
    managed-services:
      billing/ledger:
        kind: external
        external-name: ledger.example.test
        port: 5432
        egress-cidrs: [203.0.113.0/24]
    host:
      coordinate: example/staging/region-a
      component: platform-host
      domain: acme
      audience: accounts
      trust_domain: cluster.example
      envelope_revision: 1
      delivery: platform/accounts/rest
`,
		filepath.Join("modules", "shop", resources.ModuleConfigurationName):                           "kind: module\nname: shop\nservices:\n  - name: api\n",
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName):       cellServiceYAML("api", "shop"),
		filepath.Join("modules", "billing", resources.ModuleConfigurationName):                        "kind: module\nname: billing\nservices:\n  - name: ledger\n",
		filepath.Join("modules", "billing", "services", "ledger", resources.ServiceConfigurationName): cellServiceYAML("ledger", "shop"),
		filepath.Join("configurations", "staging", "shop.env"):                                        "MODE=prod\nTOKEN_LIMIT=4\n",
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

const cellDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      serviceAccountName: api
      containers:
        - name: api
          image: registry.example.test/acme/api:1.2.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        - name: proxy
          image: registry.example.test/mesh/proxy@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
`

// renderCellTree renders module's one-service tree for staging, the way a
// module render leaves it under deployments/modules.
func renderCellTree(t *testing.T, workspace *resources.Workspace, module, service, namespace string, digests map[string]string) RenderResult {
	t.Helper()
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: moduleRenderDestination(workspace, module),
		Module:      module, Environment: "staging", Namespace: namespace, AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/" + module,
		Units:                         promotableServiceGraph(module, []string{service}),
		Package:                       &InventoryPackage{ID: "acme/" + module, Version: "1.2.0"},
		WorkspaceConfigurationDigests: digests,
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", service, "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(overlay, kustomizationFile), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - deployment.yaml\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(strings.ReplaceAll(cellDeployment, "name: api", "name: "+service)), 0o644)
	})
	require.NoError(t, err)
	return result
}

// TestCellFileInventoriesEveryRenderedWorkload pins the cell file's shape: one
// namespace per rendered module, each workload with the account and SPIFFE ID
// it runs as, its pinned containers, the artifact and release it comes from, the
// endpoints it serves with their ports, the ingress into them and the egress a
// managed service grants.
func TestCellFileInventoriesEveryRenderedWorkload(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", nil)
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", nil)

	result, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"billing", "shop"}, result.Modules)
	require.Empty(t, result.Skipped)
	require.Equal(t, filepath.Join(workspace.Dir(), "deployments", "cells", "staging", "cell.yaml"), result.Path)

	data, err := os.ReadFile(result.Path)
	require.NoError(t, err)
	var cell CellFile
	require.NoError(t, yaml.Unmarshal(data, &cell))
	require.Equal(t, CellSchemaV1, cell.Schema)
	require.Equal(t, "example/staging/region-a", cell.Coordinate)
	require.Equal(t, "platform-host", cell.Component)
	require.Len(t, cell.Namespaces, 2)
	require.Equal(t, "acme-billing", cell.Namespaces[0].Name)
	shop := cell.Namespaces[1]
	require.Equal(t, "acme-shop", shop.Name)
	require.Len(t, shop.Workloads, 1)
	api := shop.Workloads[0]
	require.Equal(t, "api", api.Name)
	require.Equal(t, "shop/api", api.Service)
	require.Equal(t, "api", api.ServiceAccount)
	require.Equal(t, "spiffe://cluster.example/ns/acme-shop/sa/api", api.SPIFFEID)
	require.Equal(t, []CellContainer{
		{Name: "api", Image: CellImage{Repository: "registry.example.test/acme/api", Digest: "sha256:" + strings.Repeat("a", 64)}},
		{Name: "proxy", Image: CellImage{Repository: "registry.example.test/mesh/proxy", Digest: "sha256:" + strings.Repeat("b", 64)}},
	}, api.Containers)
	require.Equal(t, "api", api.Artifact.Name)
	require.True(t, strings.HasPrefix(api.Artifact.Digest, "sha256:"))
	require.Equal(t, &CellRelease{Publisher: "acme", Name: "shop", Version: "1.2.0"}, api.Release)
	require.Equal(t, []CellEndpoint{
		{Name: "grpc", API: "grpc", Port: 9090, Visibility: "internal", AllowModules: []string{"billing"}},
		{Name: "http", API: "http", Port: 8080, Visibility: "public"},
	}, api.Endpoints)
	require.Equal(t, []CellIngress{{Endpoint: "http", Hosts: []string{"shop.example.test"}}}, api.Ingress)
	require.Empty(t, shop.Egress)
	require.Equal(t, []CellEgress{{Service: "billing/ledger", CIDRs: []string{"203.0.113.0/24"}}}, cell.Namespaces[0].Egress)

	// The file carries no generation, no domain and no tombstone: it is an
	// inventory regenerated whole.
	for _, forbidden := range []string{"generation", "ownership_domain", "removed"} {
		require.NotContains(t, string(data), forbidden)
	}
	// A second render with nothing changed writes identical bytes.
	again, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	same, err := os.ReadFile(again.Path)
	require.NoError(t, err)
	require.Equal(t, string(data), string(same))
}

func TestCellFileSkipsTreesRenderedForAnotherEnvironment(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", nil)
	// A tree rendered for another environment describes another cell.
	other := filepath.Join(workspace.Dir(), "deployments", "modules", "billing")
	require.NoError(t, os.MkdirAll(other, 0o755))
	inventory := Inventory{SchemaVersion: SchemaVersion, Module: "billing", Environment: "production", AppProject: "acme-production", OwnedPath: "deployments/modules/billing"}
	require.NoError(t, writeCanonicalInventory(filepath.Join(other, InventoryFilename), &inventory))

	result, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, result.Modules)
	require.Equal(t, []string{"billing"}, result.Skipped)
}

// TestRenderRefusesWhenASiblingConsumerBakedInAnotherGroupValue is the
// acceptance case: change a workspace group consumed by another module, render
// only one, and the render refuses, naming the stale consumer.
func TestRenderRefusesWhenASiblingConsumerBakedInAnotherGroupValue(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	shopServices := loadServices(t, workspace, "shop", "api")
	before, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.True(t, strings.HasPrefix(before["shop"], "sha256:"))
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", before)

	// Nothing changed: the sibling agrees.
	require.NoError(t, refuseStaleGroupConsumers(workspace.Dir(), "shop", "staging", before))

	// The group changes; billing was rendered against the old value.
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "shop.env"), []byte("MODE=prod\nTOKEN_LIMIT=8\n"), 0o644))
	after, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices)
	require.NoError(t, err)
	require.NotEqual(t, before["shop"], after["shop"])
	err = refuseStaleGroupConsumers(workspace.Dir(), "shop", "staging", after)
	var stale *StaleGroupConsumersError
	require.ErrorAs(t, err, &stale)
	require.Equal(t, map[string][]string{"shop": {"billing"}}, stale.Stale)
	require.Contains(t, err.Error(), "render those modules too")

	// A sibling rendered for another environment is not this environment's
	// consumer, and a sibling that does not consume the group has no opinion.
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", nil)
	require.NoError(t, refuseStaleGroupConsumers(workspace.Dir(), "shop", "staging", after))
}

func loadServices(t *testing.T, workspace *resources.Workspace, module string, names ...string) []*resources.Service {
	t.Helper()
	services := make([]*resources.Service, 0, len(names))
	for _, name := range names {
		service, err := workspace.LoadService(context.Background(), &resources.ServiceWithModule{Name: name, Module: module})
		require.NoError(t, err)
		services = append(services, service)
	}
	return services
}

var _ = environments.ComposesSeveralModules
