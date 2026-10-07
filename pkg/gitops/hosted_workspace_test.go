package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// hostedServiceYAML is a service declaring endpoints with their ports and a
// workspace configuration group, the two things the presence document and the group
// digest read off a service.
func hostedServiceYAML(name string, groups ...string) string {
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

// writeHostedWorkspace lays down a workspace composing module "shop" with a
// service "api" that declares endpoints, ports and a configuration group, and
// an environment naming a host.
func writeHostedWorkspace(t *testing.T) *resources.Workspace {
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
		filepath.Join("modules", "shop", "services", "api", resources.ServiceConfigurationName):       hostedServiceYAML("api", "shop"),
		filepath.Join("modules", "billing", resources.ModuleConfigurationName):                        "kind: module\nname: billing\nservices:\n  - name: ledger\n",
		filepath.Join("modules", "billing", "services", "ledger", resources.ServiceConfigurationName): hostedServiceYAML("ledger", "shop") + "service-dependencies:\n  - name: api\n    module: shop\n    endpoints:\n      - name: grpc\n        api: grpc\n",
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

const hostedDeployment = `apiVersion: apps/v1
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

// renderHostedTree renders module's one-service tree for staging, the way a
// module render leaves it under deployments/modules.
func renderHostedTree(t *testing.T, workspace *resources.Workspace, module, service, namespace string, digests map[string]string) RenderResult {
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
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(strings.ReplaceAll(hostedDeployment, "name: api", "name: "+service)), 0o644)
	})
	require.NoError(t, err)
	return result
}

// TestPresenceRefusesAWorkloadWhoseAuthenticatingContainerCannotBeTold: several
// containers and none named after the service is the sidecar ambiguity, named
// in the refusal rather than resolved by a guess — through the reader the
// presence document derives its workloads with.
func TestPresenceRefusesAWorkloadWhoseAuthenticatingContainerCannotBeTold(t *testing.T) {
	workspace := writeHostedWorkspace(t)
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
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
		ambiguous := strings.Replace(hostedDeployment, "        - name: api\n          image: registry.example.test/acme/api:1.2.0", "        - name: web\n          image: registry.example.test/acme/api:1.2.0", 1)
		return os.WriteFile(filepath.Join(overlay, "deployment.yaml"), []byte(ambiguous), 0o644)
	})
	require.NoError(t, err)
	workloads, err := renderedWorkloads(filepath.Join(result.Path, "services", "api"), "staging")
	require.NoError(t, err)
	_, _, err = authenticatingContainer("api", servingWorkload(t, workloads, "api"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "none of its containers (web, proxy) is named \"api\"")
}

// TestRenderRefusesATokenASidecarCouldPresent: a projected ServiceAccount
// token minted for an explicit audience may be mounted by the authenticating
// container and no other, case for case. The token the
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

	// The refusal is the presence document's, through the reader it derives
	// its workloads with.
	workspace := writeHostedWorkspace(t)
	result, err := RenderOwnedTree(context.Background(), &RenderOptions{
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
	workloads, err := renderedWorkloads(filepath.Join(result.Path, "services", "api"), "staging")
	require.NoError(t, err)
	_, _, err = authenticatingContainer("api", servingWorkload(t, workloads, "api"))
	require.Error(t, err)
	require.Contains(t, err.Error(), `proxy mounts host-token (audience "accounts")`)
}

// TestGroupChangeIsReportedAtRenderAndRefusedAtPublish is the acceptance
// case for a shared workspace configuration group, held to the rule that every
// refusal is satisfiable: both consumers rendered against the old value, the
// group changes, and the way out is to render each — never a refusal on a
// sibling's account at render, which deadlocked (rendering A refused because
// B's tree was stale, rendering B refused because A's still was).
func TestGroupChangeIsReportedAtRenderAndRefusedAtPublish(t *testing.T) {
	workspace := writeHostedWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	ctx := context.Background()
	shopServices := loadServices(t, workspace, "shop", "api")
	before, err := workspaceConfigurationDigests(ctx, workspace, env, shopServices, nil)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.True(t, strings.HasPrefix(before["shop"], "sha256:"))
	renderHostedTree(t, workspace, "billing", "ledger", "acme-billing", before)
	renderHostedTree(t, workspace, "shop", "api", "acme-shop", before)

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
	renderHostedTree(t, workspace, "shop", "api", "acme-shop", after)
	require.NoError(t, refuseStaleRender("shop", "staging", after, after))
	err = refuseStaleGroupConsumers(workspace.Dir(), "shop", "staging", after)
	var consumers *StaleGroupConsumersError
	require.ErrorAs(t, err, &consumers)
	require.Equal(t, map[string][]string{"shop": {"billing"}}, consumers.Stale)
	require.Contains(t, err.Error(), "render those modules")

	// billing rendered at the new value: both publishes hold. No deadlock.
	renderHostedTree(t, workspace, "billing", "ledger", "acme-billing", after)
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
	workspace := writeHostedWorkspace(t)
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
	workspace := writeHostedWorkspace(t)
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

// servingWorkload picks the Deployment named name out of what a unit's overlay
// renders — a bootstrap Job may render beside it.
func servingWorkload(t *testing.T, workloads []renderedWorkload, name string) *renderedWorkload {
	t.Helper()
	for index := range workloads {
		if workloads[index].Kind == kindDeployment && workloads[index].Name == name {
			return &workloads[index]
		}
	}
	t.Fatalf("no Deployment %s among the rendered workloads: %+v", name, workloads)
	return nil
}
