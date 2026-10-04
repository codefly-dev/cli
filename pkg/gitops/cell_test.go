package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
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
    egress:
      shop/api: {hosts: [identity.example.test, api.github.com]}
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
		filepath.Join("modules", "billing", "services", "ledger", resources.ServiceConfigurationName): cellServiceYAML("ledger", "shop") + "service-dependencies:\n  - name: api\n    module: shop\n    endpoints:\n      - name: grpc\n        api: grpc\n",
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
        - name: proxy
          image: registry.example.test/mesh/proxy@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
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
	require.Equal(t, "acme", cell.Domain, "the ownership domain the composition delivers under, for the platform to hold its signer policy against")
	require.Equal(t, "cluster.example", cell.TrustDomain, "the trust domain is carried at the top level so a reader can re-derive every spiffe_id and refuse a mismatch")
	require.Len(t, cell.Namespaces, 2)
	require.Equal(t, "acme-billing", cell.Namespaces[0].Name)
	shop := cell.Namespaces[1]
	require.Equal(t, "acme-shop", shop.Name)
	require.Len(t, shop.Workloads, 2, "the bootstrap Job is a declared workload with its own image")
	api := shop.Workloads[0]
	require.Equal(t, "api", api.Name)
	require.Equal(t, kindDeployment, api.Kind)
	require.Equal(t, map[string]string{"app.kubernetes.io/name": "api"}, api.Selector)
	require.Equal(t, "shop/api", api.Service)
	require.Equal(t, "api", api.ServiceAccount)
	require.Equal(t, "api", api.Authenticating, "the container named after the service authenticates; the proxy is in the closed set that never does")
	require.False(t, api.Verifier, "the host's delivery API is another module's service")
	migrate := shop.Workloads[1]
	require.Equal(t, "api-migrate", migrate.Name)
	require.Empty(t, migrate.Endpoints, "a bootstrap Job serves none of the service's endpoints")
	require.Empty(t, migrate.Ingress, "and reaches none of its ingress")
	require.Equal(t, kindJob, migrate.Kind)
	require.Equal(t, map[string]string{"codefly.dev/bootstrap-service": "api"}, migrate.Selector, "a Job's pods are selected by its template labels, never an assumed app label")
	require.Equal(t, "sha256:"+strings.Repeat("d", 64), migrate.Containers[0].Image.Digest)
	require.Equal(t, "api", migrate.Authenticating, "a single container is the authenticating one")
	require.Equal(t, "spiffe://cluster.example/ns/acme-shop/sa/api", api.SPIFFEID)
	require.Equal(t, []CellContainer{
		{Name: "api", Image: CellImage{Repository: "registry.example.test/acme/api", Digest: "sha256:" + strings.Repeat("a", 64)}},
		{Name: "proxy", Image: CellImage{Repository: "registry.example.test/mesh/proxy", Digest: "sha256:" + strings.Repeat("b", 64)}},
	}, api.Containers)
	require.Equal(t, "api", api.Artifact.Name)
	require.True(t, strings.HasPrefix(api.Artifact.Digest, "sha256:"))
	require.Equal(t, &CellRelease{Publisher: "acme", Name: "shop", Version: "1.2.0"}, api.Release)
	require.Equal(t, []CellEndpoint{
		{Name: "grpc", API: "grpc", Port: 9090, Visibility: "internal", AllowModules: []string{"billing"}, Consumers: []string{"billing/ledger"}},
		{Name: "http", API: "http", Port: 8080, Visibility: "public"},
	}, api.Endpoints, "the grpc endpoint has a declared consumer; the http endpoint has none, visibly")
	require.Equal(t, []CellIngress{{Endpoint: "http", Hosts: []string{"shop.example.test"}}}, api.Ingress)
	require.Equal(t, []CellEgress{{Service: "shop/api", Hosts: []CellEgressHost{{Name: "api.github.com", Port: 443}, {Name: "identity.example.test", Port: 443}}}}, shop.Egress,
		"egress hosts are the environment's declaration, carried not derived, each with the port it is reached on made explicit")
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

// TestCellFileMarksTheVerifierAndCarriesTheGrants pins the fields the
// platform derives policy from beyond the mesh edges: the serving workload of
// the service the host block names as the delivery API is the verifier (its
// bootstrap Job is not), the cell bindings and cloud identity the environment
// declares per service ride on every workload of that service, and an egress
// host declared on another port keeps it.
func TestCellFileMarksTheVerifierAndCarriesTheGrants(t *testing.T) {
	workspace := writeCellWorkspace(t)
	manifest, err := os.ReadFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName))
	require.NoError(t, err)
	patched := strings.Replace(string(manifest), "delivery: platform/accounts/rest", "delivery: shop/api/http", 1)
	patched = strings.Replace(patched, "    egress:\n      shop/api: {hosts: [identity.example.test, api.github.com]}\n",
		"    egress:\n      shop/api: {hosts: [identity.example.test, {name: smtp.example.test, port: 587}]}\n    cell:\n      shop/api: {bindings: [vault, audit], cloud-identity: true}\n", 1)
	require.NotEqual(t, string(manifest), patched)
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName), []byte(patched), 0o644))
	workspace, err = resources.LoadWorkspaceFromDir(context.Background(), workspace.Dir())
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	renderCellTree(t, workspace, "shop", "api", "acme-shop", nil)
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", nil)

	result, err := RenderCell(context.Background(), workspace, env)
	require.NoError(t, err)
	data, err := os.ReadFile(result.Path)
	require.NoError(t, err)
	var cell CellFile
	require.NoError(t, yaml.Unmarshal(data, &cell))
	shop := cell.Namespaces[1]
	require.Equal(t, "acme-shop", shop.Name)
	api, migrate := shop.Workloads[0], shop.Workloads[1]
	require.Equal(t, "api", api.Name)
	require.True(t, api.Verifier, "the serving workload of the delivery API's service verifies delivered documents")
	require.Equal(t, "api-migrate", migrate.Name)
	require.False(t, migrate.Verifier, "a bootstrap Job of that service runs its own image and verifies nothing")
	for _, workload := range []CellWorkload{api, migrate} {
		require.Equal(t, []string{"audit", "vault"}, workload.Bindings, "the cell bindings the service declares ride on every workload of it, sorted")
		require.True(t, workload.CloudIdentity)
	}
	ledger := cell.Namespaces[0].Workloads[0]
	require.False(t, ledger.Verifier)
	require.Nil(t, ledger.Bindings)
	require.False(t, ledger.CloudIdentity)
	require.Equal(t, []CellEgressHost{{Name: "identity.example.test", Port: 443}, {Name: "smtp.example.test", Port: 587}}, shop.Egress[0].Hosts)
	// The YAML spells every one of them, so a reader never infers.
	for _, want := range []string{"trust_domain: cluster.example", "verifier: true", "authenticating: api", "cloud_identity: true", "- vault", "port: 587"} {
		require.Contains(t, string(data), want)
	}
}

// TestCellFileRefusesAWorkloadWhoseAuthenticatingContainerCannotBeTold: several
// containers and none named after the service is the sidecar ambiguity, named
// in the refusal rather than resolved by a guess.
func TestCellFileRefusesAWorkloadWhoseAuthenticatingContainerCannotBeTold(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: moduleRenderDestination(workspace, "shop"),
		Module:      "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop",
		Units:   promotableServiceGraph("shop", []string{"api"}),
		Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		ambiguous := strings.Replace(cellDeployment, "        - name: api\n          image: registry.example.test/acme/api:1.2.0", "        - name: web\n          image: registry.example.test/acme/api:1.2.0", 1)
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(ambiguous), 0o644)
	})
	require.NoError(t, err)
	_, err = RenderCell(context.Background(), workspace, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "none of its containers (web, proxy) is named \"api\"")
}

// TestRenderRefusesATokenASidecarCouldPresent mirrors the cell's admission
// rule at publish, case for case as the cell verified it against an API
// server: a projected ServiceAccount token minted for an explicit audience may
// be mounted by the authenticating container and no other. The token the
// ServiceAccount plugin injects is projected too, mounted everywhere, and names
// no audience, so the rule keys on the audience — a rule keyed on "a projected
// token volume" would refuse every multi-container pod.
func TestRenderRefusesATokenASidecarCouldPresent(t *testing.T) {
	deployment := func(apiMounts, proxyMounts, initMounts string) string {
		return `apiVersion: apps/v1
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
      initContainers:
        - name: migrate
          image: registry.example.test/acme/api-migrate@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
` + initMounts + `
      containers:
        - name: api
          image: registry.example.test/acme/api:1.2.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
` + apiMounts + `
        - name: proxy
          image: registry.example.test/mesh/proxy@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
` + proxyMounts + `
      volumes:
        - name: kube-api-access-x7k2p
          projected:
            sources:
              - serviceAccountToken:
                  path: token
        - name: host-token
          projected:
            sources:
              - serviceAccountToken:
                  audience: accounts
                  expirationSeconds: 600
                  path: token
`
	}
	const injected = "          volumeMounts:\n            - name: kube-api-access-x7k2p\n              mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n"
	const audience = "          volumeMounts:\n            - name: host-token\n              mountPath: /var/run/secrets/codefly/host\n"
	const both = "          volumeMounts:\n            - name: kube-api-access-x7k2p\n              mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n            - name: host-token\n              mountPath: /var/run/secrets/codefly/host\n"
	cases := []struct {
		name             string
		api, proxy, init string
		refused          string
	}{
		{name: "the injected token in every container", api: injected, proxy: injected, init: injected},
		{name: "the audience token in the authenticating container only", api: both, proxy: injected},
		{name: "the audience token also in a sidecar", api: both, proxy: both, refused: `proxy mounts host-token (audience "accounts")`},
		{name: "the audience token in the sidecar alone", api: injected, proxy: audience, refused: `proxy mounts host-token (audience "accounts")`},
		{name: "the audience token in an init container", api: audience, init: audience, refused: `migrate mounts host-token (audience "accounts")`},
		// A declared volume nobody mounts is projected into no filesystem, so no
		// container can present it: inert, and ADMITTED. The cell's rule is
		// one-sided by construction — the authenticating container is exempted
		// and nothing requires it to mount the volume — and the render matches
		// it exactly; "completing" either side into an equality would make the
		// publisher and the enforcer disagree, and the failure would land on
		// whoever deployed the pod rather than on either of us.
		{name: "the audience token volume mounted by nobody", api: injected, proxy: injected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unit := t.TempDir()
			overlay := filepath.Join(unit, "overlays", "staging")
			require.NoError(t, os.MkdirAll(overlay, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(deployment(tc.api, tc.proxy, tc.init)), 0o644))
			workloads, err := renderedWorkloads(unit, "staging")
			require.NoError(t, err)
			require.Len(t, workloads, 1)
			authenticating, others, err := authenticatingContainer("api", &workloads[0])
			if tc.refused == "" {
				require.NoError(t, err)
				require.Equal(t, "api", authenticating.Name)
				require.Equal(t, []string{"migrate", "proxy"}, others)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.refused)
			require.Contains(t, err.Error(), `mount it into "api" alone`)
		})
	}

	// The refusal reaches the cell file and the presence document alike, since
	// both derive the authenticating container through the same function.
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: moduleRenderDestination(workspace, "shop"),
		Module:      "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop",
		Units:   promotableServiceGraph("shop", []string{"api"}),
		Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(deployment(both, both, "")), 0o644)
	})
	require.NoError(t, err)
	_, err = RenderCell(context.Background(), workspace, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), `proxy mounts host-token (audience "accounts")`)
}

// TestCellFileInventoriesAManagedUnitsBootstrapJobAndQualifiesIngress: a
// managed service's bootstrap Job is a pod the closed admission set refuses
// unless the cell names it, so it is inventoried like any workload, with no
// endpoints of its own; and an ingress route reaches the service it names
// module-qualified, never every module's service of that bare name.
func TestCellFileInventoriesAManagedUnitsBootstrapJobAndQualifiesIngress(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	// shop/api serves and has the public ingress; billing/ledger also runs
	// a service named "api"-like enough to be confused — rendered under the
	// bare name "api" in another module — and a managed "store" bootstrap.
	renderCellTree(t, workspace, "shop", "api", "acme-shop", nil)
	result, err := RenderOwnedTree(ctx, &RenderOptions{
		Destination: moduleRenderDestination(workspace, "billing"),
		Module:      "billing", Environment: "staging", Namespace: "acme-billing", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/billing",
		Units: append(promotableServiceGraph("billing", []string{"ledger"}),
			InventoryUnit{Kind: UnitKindService, Module: "billing", Name: "store", Path: "services/store", Managed: true, Bootstrap: true}),
		Package: &InventoryPackage{ID: "acme/billing", Version: "1.2.0"},
	}, func(_ context.Context, root string) error {
		for name, body := range map[string]string{
			"ledger": strings.ReplaceAll(cellDeployment, "name: api", "name: ledger"),
			"store": `apiVersion: batch/v1
kind: Job
metadata:
  name: store-bootstrap
spec:
  template:
    metadata:
      labels:
        codefly.dev/bootstrap-service: store
    spec:
      serviceAccountName: store-bootstrap
      restartPolicy: OnFailure
      containers:
        - name: store-bootstrap
          image: registry.example.test/managed/postgres-init@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
`,
		} {
			overlay := filepath.Join(root, "services", name, "overlays", "staging")
			if err := os.MkdirAll(overlay, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(body), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, result.Inventory.Units, 2)

	cell, err := RenderCell(ctx, workspace, env)
	require.NoError(t, err)
	data, err := os.ReadFile(cell.Path)
	require.NoError(t, err)
	var file CellFile
	require.NoError(t, yaml.Unmarshal(data, &file))
	billing := file.Namespaces[0]
	require.Equal(t, "acme-billing", billing.Name)
	names := map[string]CellWorkload{}
	for _, workload := range billing.Workloads {
		names[workload.Name] = workload
	}
	bootstrap, declared := names["store-bootstrap"]
	require.True(t, declared, "the managed unit's bootstrap Job is a pod admission must know: %v", names)
	require.Equal(t, "Job", bootstrap.Kind)
	require.Equal(t, "billing/store", bootstrap.Service)
	require.Equal(t, "store-bootstrap", bootstrap.Authenticating)
	require.Empty(t, bootstrap.Endpoints, "a managed unit declares no endpoints of its own")
	require.Equal(t, "sha256:"+strings.Repeat("e", 64), bootstrap.Containers[0].Image.Digest)
	for _, workload := range billing.Workloads {
		require.Empty(t, workload.Ingress, "shop's public ingress names shop/api; nothing in billing inherits it")
	}
	shop := file.Namespaces[1]
	require.Equal(t, "acme-shop", shop.Name)
	require.Len(t, shop.Workloads[0].Ingress, 1)
}

// TestCellFileDeclaresThePresenceDeliveryJob: the Job that POSTs a module's
// presence documents is a pod in the module's namespace, running as the
// delivery account, that the closed admission set would refuse unless the
// cell names it. Its name carries the settled set's digest and is decided at
// publish, so it is declared by the labels its pods carry.
func TestCellFileDeclaresThePresenceDeliveryJob(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "shop-api@example.iam.test"}}
	_, err := RenderOwnedTree(ctx, &RenderOptions{
		Destination: moduleRenderDestination(workspace, "shop"),
		Module:      "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop", Workspace: "acme", Host: env.Host,
		Units:   promotableServiceGraph("shop", []string{"api"}),
		Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
		SolutionInstances: []SolutionInstance{{
			Kind: solutionhost.KindModule, Name: "shop", Package: "acme/shop", Version: "1.2.0",
			ReleaseDigest: testReleaseDigest, Units: units,
		}},
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(cellDeployment), 0o644)
	})
	require.NoError(t, err)
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", nil)

	cell, err := RenderCell(ctx, workspace, env)
	require.NoError(t, err)
	data, err := os.ReadFile(cell.Path)
	require.NoError(t, err)
	var file CellFile
	require.NoError(t, yaml.Unmarshal(data, &file))
	billing, shop := file.Namespaces[0], file.Namespaces[1]
	require.Nil(t, billing.Delivery, "a module delivering no presence runs no delivery Job")
	require.NotNil(t, shop.Delivery)
	require.Equal(t, &CellDelivery{
		Kind:           "Job",
		Selector:       map[string]string{"app.kubernetes.io/managed-by": "codefly", "codefly.dev/delivery": "presence"},
		ServiceAccount: "delivery",
		SPIFFEID:       "spiffe://cluster.example/ns/acme-shop/sa/delivery",
		Container:      "deliver",
		Image:          CellImage{Repository: "curlimages/curl:8.18.0", Digest: "sha256:d94d07ba9e7d6de898b6d96c1a072f6f8266c687af78a74f380087a0addf5d17"},
	}, shop.Delivery)
	require.Contains(t, string(data), "delivery:\n", "the YAML spells it, so a loader never infers it")
}

// TestCellFileRefusesASelectorItCannotCarry: the cell carries the exact label
// set a policy selects pods by; a selector written with matchExpressions
// would be reduced to its matchLabels — or to {} — and a policy derived from
// that selects either nothing or everything. Refused at render, by name.
func TestCellFileRefusesASelectorItCannotCarry(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: moduleRenderDestination(workspace, "shop"),
		Module:      "shop", Environment: "staging", Namespace: "acme-shop", AppProject: "acme-staging",
		Promotable: true, OwnedPath: "deployments/modules/shop",
		Units:   promotableServiceGraph("shop", []string{"api"}),
		Package: &InventoryPackage{ID: "acme/shop", Version: "1.2.0"},
	}, func(_ context.Context, root string) error {
		overlay := filepath.Join(root, "services", "api", "overlays", "staging")
		if err := os.MkdirAll(overlay, 0o755); err != nil {
			return err
		}
		expressions := strings.Replace(cellDeployment, "  selector:\n    matchLabels:\n      app.kubernetes.io/name: api\n",
			"  selector:\n    matchExpressions:\n      - key: app.kubernetes.io/name\n        operator: In\n        values: [api]\n", 1)
		if expressions == cellDeployment {
			return errors.New("fixture selector not found")
		}
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(expressions), 0o644)
	})
	require.NoError(t, err)
	_, err = RenderCell(context.Background(), workspace, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "workload api selects its pods with matchExpressions, which the cell file cannot carry")
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

// TestGroupChangeIsReportedAtRenderAndRefusedAtPublish is the acceptance
// case for a shared workspace configuration group, held to the rule that every
// refusal is satisfiable: both consumers rendered against the old value, the
// group changes, and the way out is to render each — never a refusal on a
// sibling's account at render, which deadlocked (rendering A refused because
// B's tree was stale, rendering B refused because A's still was).
func TestGroupChangeIsReportedAtRenderAndRefusedAtPublish(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	shopServices := loadServices(t, workspace, "shop", "api")
	before, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices, nil)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.True(t, strings.HasPrefix(before["shop"], "sha256:"))
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", before)
	renderCellTree(t, workspace, "shop", "api", "acme-shop", before)

	// Nothing changed: no sibling is stale, and the publish of either holds.
	stale, err := staleGroupConsumers(workspace.Dir(), "shop", "staging", before)
	require.NoError(t, err)
	require.Empty(t, stale)
	require.NoError(t, refuseStaleRender("shop", "staging", before, before))

	// The group changes. Both trees are now rendered against the old value:
	// publishing either is refused because ITS OWN render is stale — the
	// action is to render it — and the render of shop then reports billing
	// as stale rather than refusing on its account.
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "shop.env"), []byte("MODE=prod\nTOKEN_LIMIT=8\n"), 0o644))
	after, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices, nil)
	require.NoError(t, err)
	require.NotEqual(t, before["shop"], after["shop"])
	err = refuseStaleRender("shop", "staging", before, after)
	require.Error(t, err)
	require.Contains(t, err.Error(), "render shop again")
	stale, err = staleGroupConsumers(workspace.Dir(), "shop", "staging", after)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"shop": {"billing"}}, stale, "the render reports billing; it does not refuse")

	// shop rendered at the new value: its own render is current, and its
	// publish is refused only until billing is rendered too — by name.
	renderCellTree(t, workspace, "shop", "api", "acme-shop", after)
	require.NoError(t, refuseStaleRender("shop", "staging", after, after))
	err = refuseStaleGroupConsumers(workspace.Dir(), "shop", "staging", after)
	var consumers *StaleGroupConsumersError
	require.ErrorAs(t, err, &consumers)
	require.Equal(t, map[string][]string{"shop": {"billing"}}, consumers.Stale)
	require.Contains(t, err.Error(), "render those modules")

	// billing rendered at the new value: both publishes hold. No deadlock.
	renderCellTree(t, workspace, "billing", "ledger", "acme-billing", after)
	require.NoError(t, refuseStaleGroupConsumers(workspace.Dir(), "shop", "staging", after))
	require.NoError(t, refuseStaleGroupConsumers(workspace.Dir(), "billing", "staging", after))

	// An unreadable sibling inventory is an error, never "it agrees".
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "deployments", "modules", "billing", InventoryFilename), []byte("{not json"), 0o644))
	_, err = staleGroupConsumers(workspace.Dir(), "shop", "staging", after)
	require.Error(t, err)
	require.Contains(t, err.Error(), "the rendered tree of module billing cannot be read")
}

// TestGroupDigestsCoverContractSlots: a contract slot {from: <group>/<key>}
// is baked into the delivered authority document, so its group is digested
// as a service's dependency is — a change to it is a change the publish holds
// the render to.
func TestGroupDigestsCoverContractSlots(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	shopServices := loadServices(t, workspace, "shop", "api")
	withoutSlots, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices, nil)
	require.NoError(t, err)
	require.NotContains(t, withoutSlots, "assistant")
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations", "staging", "assistant.env"), []byte("MODEL_AUDIENCE=model-gateway\n"), 0o644))
	withSlots, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices, []string{"assistant"})
	require.NoError(t, err)
	require.Contains(t, withSlots, "assistant")
	require.Equal(t, withoutSlots["shop"], withSlots["shop"])
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

// TestPublishRefusesAStaleConsumerWithoutItsCurrentRender: a consumer the
// base branch delivers at a stale group digest is refused unless this
// workspace holds its current render; a current render lifts it.
func TestPublishRefusesAStaleConsumerWithoutItsCurrentRender(t *testing.T) {
	workspace := t.TempDir()
	stale := []staleBaseConsumer{{Module: "billing", Group: "shared", Current: "abc"}}
	err := refuseStaleBaseConsumers(workspace, "prod", "main", stale)
	require.Error(t, err)
	require.Contains(t, err.Error(), "billing (group shared)")

	dir := filepath.Join(workspace, "deployments", "modules", "billing")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	inventory := Inventory{SchemaVersion: SchemaVersion, Module: "billing", Environment: "prod", WorkspaceConfigurationDigests: map[string]string{"shared": "old"}}
	require.NoError(t, writeCanonicalInventory(filepath.Join(dir, InventoryFilename), &inventory))
	err = refuseStaleBaseConsumers(workspace, "prod", "main", stale)
	require.Error(t, err, "a stale local render is no evidence")

	inventory.WorkspaceConfigurationDigests["shared"] = "abc"
	require.NoError(t, writeCanonicalInventory(filepath.Join(dir, InventoryFilename), &inventory))
	require.NoError(t, refuseStaleBaseConsumers(workspace, "prod", "main", stale))

	// A sibling directory with no inventory is unreadable evidence, never
	// agreement.
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "deployments", "modules", "ledger"), 0o755))
	_, err = staleGroupConsumers(workspace, "billing", "prod", map[string]string{"shared": "abc"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ledger carries no inventory")
}

// TestPublishHoldsTheRenderToTheGroupsConsumedNow: the groups a publish holds
// the render to are derived from the composition at publish, so a group the
// module's services consume now and the render did not record is a stale
// render — the recorded digests alone could not say the group exists.
func TestPublishHoldsTheRenderToTheGroupsConsumedNow(t *testing.T) {
	workspace := writeCellWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	current, err := currentGroupDigests(ctx, workspace, env, module)
	require.NoError(t, err)
	require.Contains(t, current, "shop")
	err = refuseStaleRender("shop", "staging", map[string]string{}, current)
	require.Error(t, err, "a render that recorded no digest for a group consumed now is stale")
	require.Contains(t, err.Error(), "shop")
	require.NoError(t, refuseStaleRender("shop", "staging", current, current))
}
