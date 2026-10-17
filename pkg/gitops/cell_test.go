package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func cellServiceYAML(name string) string {
	return devServiceYAML(name) + `endpoints:
  - name: grpc
    api: grpc
  - name: http
    api: http
`
}

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
    egress:
      shop/api: {hosts: [identity.example.test, {name: api.github.com, port: 8443}]}
    cell:
      shop/api: {bindings: [vault, identity], cloud-identity: true, presenter: [http]}
    host:
      coordinate: region-a
      component: platform-host
      domain: acme
      audience: accounts
      trust_domain: cluster.example
      delivery: billing/ledger/http
`,
		filepath.Join("modules", "shop", resources.ModuleConfigurationName):                           "kind: module\nname: shop\nservices:\n  - name: api\n",
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName):       cellServiceYAML("api"),
		filepath.Join("modules", "billing", resources.ModuleConfigurationName):                        "kind: module\nname: billing\nservices:\n  - name: ledger\n",
		filepath.Join("modules", "billing", "services", "ledger", resources.ServiceConfigurationName): cellServiceYAML("ledger") + "service-dependencies:\n  - name: api\n    module: shop\n    endpoints:\n      - name: grpc\n",
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
  selector:
    matchLabels:
      app.kubernetes.io/name: api
  template:
    metadata:
      labels:
        app.kubernetes.io/name: api
    spec:
      serviceAccountName: api
      containers:
        - name: api
          image: registry.example.test/acme/api:1.2.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
          ports:
            - name: grpc
              containerPort: 9090
            - name: http
              containerPort: 3000
        - name: proxy
          image: registry.example.test/mesh/proxy@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
---
apiVersion: v1
kind: Service
metadata:
  name: api
spec:
  selector:
    app.kubernetes.io/name: api
  ports:
    - name: grpc
      port: 9090
      targetPort: grpc
    - name: http
      port: 8080
      targetPort: http
---
apiVersion: batch/v1
kind: Job
metadata:
  name: api-migrate
spec:
  template:
    metadata:
      labels:
        codefly.dev/bootstrap-service: api
    spec:
      serviceAccountName: api
      restartPolicy: OnFailure
      containers:
        - name: api
          image: registry.example.test/acme/api-migrate@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
`

// unitsWithPorts is the unit graph as the render records it for a hosted
// environment: each endpoint with the Service port and container port the
// render resolved, which the cell file reads back.
func unitsWithPorts(module, service string) []InventoryUnit {
	units := promotableServiceGraph(module, []string{service})
	for index := range units {
		if units[index].Name == service {
			units[index].Endpoints = []InventoryEndpoint{{Name: "grpc", Port: 9090, TargetPort: 9090}, {Name: "http", Port: 8080, TargetPort: 3000}}
		}
	}
	return units
}

func renderCellTree(t *testing.T, workspace *resources.Workspace, module, service, namespace, environment string) RenderResult {
	t.Helper()
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: moduleRenderDestination(workspace, module),
		Module:      module, Environment: environment, Namespace: namespace, AppProject: "acme-" + environment,
		Promotable: true, OwnedPath: "deployments/modules/" + module,
		Units:   unitsWithPorts(module, service),
		Package: &InventoryPackage{ID: "acme/" + module, Version: "1.2.0"},
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", service, "overlays", environment)
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

// The cell file's shape, as its consumer reads it: one namespace per rendered
// module; each workload with the account and SPIFFE ID it runs as, its pinned
// containers, the release it comes from, the endpoints it serves with the
// container port the mesh authorizes and the Service port an edge route uses,
// who the composition says calls them, the ingress into them, the bindings
// the environment declares, and the egress a service is granted.
func TestCellFileInventoriesEveryRenderedWorkload(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	result, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"billing", "shop"}, result.Modules)
	require.Empty(t, result.Skipped)
	require.False(t, result.Source, "a workspace that is not a git checkout names no source commit, and says so")
	require.Equal(t, filepath.Join(workspace.Dir(), "deployments", "cells", "staging", "cell.yaml"), result.Path)
	data, err := os.ReadFile(result.Path)
	require.NoError(t, err)
	file, err := cellfile.Parse(data)
	require.NoError(t, err)
	require.Equal(t, "region-a", file.Coordinate)
	require.Equal(t, "platform-host", file.Component)
	require.Equal(t, "acme", file.Domain)
	require.Equal(t, "cluster.example", file.TrustDomain, "carried at the top level so a reader re-derives every spiffe_id")
	require.Nil(t, file.Source)
	require.Len(t, file.Namespaces, 2)
	require.Equal(t, "acme-billing", file.Namespaces[0].Name)
	shop := file.Namespaces[1]
	require.Equal(t, "acme-shop", shop.Name)
	require.Len(t, shop.Workloads, 2, "the bootstrap Job is a declared workload with its own image")
	api := shop.Workloads[0]
	require.Equal(t, "api", api.Name)
	require.Equal(t, kindDeployment, api.Kind)
	require.Equal(t, map[string]string{"app.kubernetes.io/name": "api"}, api.Selector)
	require.Equal(t, "shop/api", api.Service)
	require.Equal(t, "api", api.ServiceAccount)
	require.Equal(t, "spiffe://cluster.example/ns/acme-shop/sa/api", api.SPIFFEID)
	require.Equal(t, "api", api.Authenticating, "the container named after the service authenticates; the proxy never does")
	require.False(t, api.Verifier, "the host's delivery API is another module's service")
	require.Equal(t, []string{"identity", "vault"}, api.Bindings, "the environment's cell declaration, sorted")
	require.True(t, api.CloudIdentity)
	require.Equal(t, []cellfile.Container{
		{Name: "api", Image: cellfile.Image{Repository: "registry.example.test/acme/api", Digest: "sha256:" + strings.Repeat("a", 64)}},
		{Name: "proxy", Image: cellfile.Image{Repository: "registry.example.test/mesh/proxy", Digest: "sha256:" + strings.Repeat("b", 64)}},
	}, api.Containers, "a tag is dropped from the repository; the digest is the identity")
	require.Equal(t, &cellfile.Release{Publisher: "acme", Name: "shop", Version: "1.2.0"}, api.Release)
	require.Equal(t, []cellfile.Endpoint{
		{Name: "grpc", API: "grpc", Port: 9090, ServicePort: 9090, Consumers: []string{"billing/ledger", "shop/api"}},
		{Name: "http", API: "http", Port: 3000, ServicePort: 8080, Consumers: []string{"shop/api"}, Presenter: true},
	}, api.Endpoints, "port is the container port the mesh authorizes, service_port the Service's, both as the render resolved and recorded them; the grpc endpoint has a declared consumer; a service that renders a bootstrap Job names itself on every endpoint, so the Job's identity is admitted to the service it bootstraps; and the endpoint the environment declares as a presenter carries that fact for the cell to front it with a protected waypoint")
	require.Equal(t, []cellfile.Ingress{{Endpoint: "http", Hosts: []string{"shop.example.test"}}}, api.Ingress)
	migrate := shop.Workloads[1]
	require.Equal(t, "api-migrate", migrate.Name)
	require.Equal(t, kindJob, migrate.Kind)
	require.Equal(t, map[string]string{"codefly.dev/bootstrap-service": "api"}, migrate.Selector, "a Job's pods are selected by its template labels")
	require.Empty(t, migrate.Endpoints, "a bootstrap Job serves none of the service's endpoints")
	require.Empty(t, migrate.Ingress)
	require.Equal(t, "api", migrate.Authenticating, "a single container is the authenticating one")
	require.Equal(t, []cellfile.Egress{{Service: "shop/api", Hosts: []cellfile.EgressHost{{Name: "api.github.com", Port: 8443}, {Name: "identity.example.test", Port: 443}}}}, shop.Egress,
		"egress hosts are the environment's declaration, each with the port it is reached on made explicit")
	require.Equal(t, []cellfile.Egress{{Service: "billing/ledger", CIDRs: []string{"203.0.113.0/24"}}}, file.Namespaces[0].Egress)
	ledger := file.Namespaces[0].Workloads[0]
	require.True(t, ledger.Verifier, "the serving workload of the host's delivery endpoint is the verifier")
	require.False(t, file.Namespaces[0].Workloads[1].Verifier, "its bootstrap Job is not")
	for _, forbidden := range []string{"generation", "ownership_domain", "removed", "visibility", "allow_modules"} {
		require.NotContains(t, string(data), forbidden, "an inventory regenerated whole carries no generation or tombstone, and nothing its reader refuses")
	}
	again, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	same, err := os.ReadFile(again.Path)
	require.NoError(t, err)
	require.Equal(t, string(data), string(same), "a second render with nothing changed writes identical bytes")
}

// A tree rendered for another environment describes another cell: it is left
// out and reported, never merged in.
func TestCellFileSkipsTreesRenderedForAnotherEnvironment(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "production")
	result, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, result.Modules)
	require.Equal(t, []string{"billing"}, result.Skipped)
}

// A workload whose authenticating container cannot be told from a sidecar is
// refused by name rather than guessed.
func TestCellFileRefusesAWorkloadWhoseAuthenticatingContainerCannotBeTold(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	result := renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	rendered := filepath.Join(result.Path, "services", "api", "overlays", "staging", "deployment.yaml")
	data, err := os.ReadFile(rendered)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rendered, []byte(strings.Replace(string(data), "- name: api\n          image: registry.example.test/acme/api:", "- name: web\n          image: registry.example.test/acme/api:", 1)), 0o644))
	_, err = RenderCell(context.Background(), workspace, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot be told from a sidecar")
}

// A sibling tree rendered by another CLI (another inventory schema) is a tree
// to render again: the stale-consumer walk names it on every group the module
// consumes instead of refusing, so a schema move converges module by module,
// and the cell file says which render it is still owed.
func TestAnOlderSiblingRenderIsReportedStaleNotFatal(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", "staging")
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", "staging")
	older := filepath.Join(moduleRenderDestination(workspace, "billing"), InventoryFilename)
	data, err := os.ReadFile(older)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(older, []byte(strings.Replace(string(data), `"schemaVersion": `+strconvItoa(SchemaVersion), `"schemaVersion": 7`, 1)), 0o644))
	stale, err := staleGroupConsumers(workspace.Dir(), "shop", "staging", map[string]string{"shop": "d1", "shared": "d2"})
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"shop": {"billing"}, "shared": {"billing"}}, stale)
	_, err = RenderCell(context.Background(), workspace, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "render billing again for staging")
	require.ErrorIs(t, err, ErrUnsupportedInventorySchema)
}

func strconvItoa(value int) string { return fmt.Sprint(value) }

// A Job an agent renders without pod labels is labelled at render with its
// name and kind, so the cell file carries a selector for it; a labelled Job
// keeps its own labels.
func TestRenderLabelsAnUnlabelledJobTemplateSoTheCellCanSelectIt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "job.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: evidence\nspec:\n  template:\n    spec:\n      containers:\n        - name: evidence\n          image: r/e@sha256:"+strings.Repeat("a", 64)+"\n---\napiVersion: batch/v1\nkind: Job\nmetadata:\n  name: labelled\nspec:\n  template:\n    metadata:\n      labels:\n        app: labelled\n    spec:\n      containers:\n        - name: labelled\n          image: r/l@sha256:"+strings.Repeat("b", 64)+"\n"), 0o644))
	require.NoError(t, labelPodTemplates(context.Background(), root))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	manifests, _, err := decodeYAML("job.yaml", data)
	require.NoError(t, err)
	evidence, err := podSelector(manifests, "evidence", kindJob)
	require.NoError(t, err)
	require.Equal(t, map[string]string{workloadLabel: "evidence", workloadKindLabel: "job"}, evidence)
	labelled, err := podSelector(manifests, "labelled", kindJob)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"app": "labelled"}, labelled)
}

// writeConfiguredWorkspace composes shop/api (grpc, http), billing/ledger,
// which declares the platform group, and audit/reader, which does not; the
// staging platform group carries gateway=<value>.
func writeConfiguredWorkspace(t *testing.T, value string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                                          "name: acme\nlayout: modules\nmodules:\n  - name: shop\n  - name: billing\n  - name: audit\nenvironments:\n  - name: staging\n    namespace: acme\n",
		filepath.Join("configurations", "staging", "platform.env"):                                    "gateway=" + value + "\n",
		filepath.Join("modules", "shop", resources.ModuleConfigurationName):                           "kind: module\nname: shop\nservices:\n  - name: api\n",
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName):       cellServiceYAML("api"),
		filepath.Join("modules", "billing", resources.ModuleConfigurationName):                        "kind: module\nname: billing\nservices:\n  - name: ledger\n",
		filepath.Join("modules", "billing", "services", "ledger", resources.ServiceConfigurationName): cellServiceYAML("ledger") + "workspace-configuration-dependencies:\n  - platform\n",
		filepath.Join("modules", "audit", resources.ModuleConfigurationName):                          "kind: module\nname: audit\nservices:\n  - name: reader\n",
		filepath.Join("modules", "audit", "services", "reader", resources.ServiceConfigurationName):   cellServiceYAML("reader"),
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

// A service that reads ${endpoint:…} through a group it declares dials that
// endpoint, so it is the endpoint's consumer exactly as a service dependency
// would make it; a service that does not declare the group is not.
func TestAConfiguredEndpointReferenceIsAConsumerEdge(t *testing.T) {
	workspace := writeConfiguredWorkspace(t, "${endpoint:shop/api/http}")
	consumers, configured, err := endpointConsumers(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.NoError(t, err)
	require.Equal(t, []string{"billing/ledger"}, consumers["shop/api/http"], "audit/reader declares no platform group")
	require.Empty(t, consumers["shop/api/grpc"], "the reference names one endpoint, not the service")
	require.Equal(t, []string{"cell: billing/ledger reaches shop/api/http through platform/gateway; declared by the configuration, not by a service dependency"}, configured)
}

func TestAConfiguredReferenceToAnUnknownEndpointIsRefused(t *testing.T) {
	workspace := writeConfiguredWorkspace(t, "http://${endpoint:shop/api/workload}")
	_, _, err := endpointConsumers(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.EqualError(t, err, "service billing/ledger reads platform/gateway = ${endpoint:shop/api/workload}, but service shop/api declares no such endpoint")
	workspace = writeConfiguredWorkspace(t, "${endpoint:shop/missing/http}")
	_, _, err = endpointConsumers(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.ErrorContains(t, err, "but the workspace composes no service shop/missing")
}
