package environments_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/stretchr/testify/require"
)

// writeExclusionWorkspace lays down module "shop": api depends on vault, and
// cache stands alone. staging carries declarations; local carries none.
func writeExclusionWorkspace(t *testing.T, declarations string) *resources.Workspace {
	t.Helper()
	return writeExclusionWorkspaceExporting(t, declarations, "")
}

// writeExclusionWorkspaceExporting is writeExclusionWorkspace with the module
// interface given.
func writeExclusionWorkspaceExporting(t *testing.T, declarations, moduleInterface string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	service := func(name, extra string) string {
		return `kind: service
name: ` + name + `
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
` + extra
	}
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: acme
layout: modules
modules:
  - name: shop
environments:
  - name: local
  - name: staging
    namespace: acme
` + declarations,
		"modules/shop/module.codefly.yaml": `kind: module
name: shop
` + moduleInterface + `services:
    - name: api
    - name: vault
    - name: cache
`,
		"modules/shop/services/api/service.codefly.yaml": service("api", `service-dependencies:
  - name: vault
`),
		"modules/shop/services/vault/service.codefly.yaml": service("vault", ""),
		"modules/shop/services/cache/service.codefly.yaml": service("cache", "endpoints:\n  - name: tcp\n    api: tcp\n"),
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

const withdrawVaultFromAPI = `    exclude-services:
      - service: shop/vault
        withdrawn-from: [shop/api]
`

// An excluded service whose every consumer is a declared withdrawal is
// admitted, and only the environment that declares it excludes it.
func TestExcludeServicesAdmitsADeclaredWithdrawal(t *testing.T) {
	workspace := writeExclusionWorkspace(t, withdrawVaultFromAPI)
	require.NoError(t, environments.ValidateWorkspace(context.Background(), workspace))

	staging, err := environments.Select(workspace, "staging")
	require.NoError(t, err)
	require.True(t, staging.Excludes("shop", "vault"))
	require.False(t, staging.Excludes("shop", "api"))
	require.Equal(t, []string{"shop/vault"}, staging.ExcludedServiceUniques())

	local, err := environments.Select(workspace, "local")
	require.NoError(t, err)
	require.False(t, local.Excludes("shop", "vault"), "another environment still composes the service")

	// The one list every module-wide loop (render, build, deploy) reads.
	module, err := workspace.LoadModuleFromName(context.Background(), "shop")
	require.NoError(t, err)
	var composed []string
	for _, reference := range staging.ComposedServices(module) {
		composed = append(composed, reference.Name)
	}
	require.Equal(t, []string{"api", "cache"}, composed)
	require.Len(t, local.ComposedServices(module), 3)

	unconsumed := writeExclusionWorkspace(t, `    exclude-services:
      - service: shop/cache
`)
	require.NoError(t, environments.ValidateWorkspace(context.Background(), unconsumed))
}

// Nothing may still consume what is excluded: every refusal names the service
// and what holds on to it, so the render can never produce a broken graph.
func TestExcludeServicesRefusesWhatStillConsumesIt(t *testing.T) {
	for name, tc := range map[string]struct{ declarations, want string }{
		"an undeclared consumer": {`    exclude-services:
      - service: shop/vault
`, "shop/api declares a dependency on it"},
		"a withdrawal from a non-consumer": {`    exclude-services:
      - service: shop/vault
        withdrawn-from: [shop/api, shop/cache]
`, "withdrawn-from names shop/cache, which declares no dependency on it"},
		"a service the workspace does not compose": {`    exclude-services:
      - service: shop/vaults
`, `"shop/vaults", which the workspace does not compose`},
		"a declaration naming it": {withdrawVaultFromAPI + `    service-identity:
      services:
        shop/vault:
          principal: system:serviceaccount:acme:vault
`, "service-identity names shop/vault"},
		"a managed replacement of it": {withdrawVaultFromAPI + `    managed-services:
      shop/vault:
        kind: vault
        external-name: vault.example
        port: 8200
`, "managed-services names shop/vault"},
		"an ingress route to it": {withdrawVaultFromAPI + `    ingress:
      - name: vault
        service: shop/vault
        endpoint: http
        hosts: [vault.example]
`, `ingress route "vault" names shop/vault`},
	} {
		t.Run(name, func(t *testing.T) {
			err := environments.ValidateWorkspace(context.Background(), writeExclusionWorkspace(t, tc.declarations))
			require.Error(t, err)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, `environment "staging"`)
		})
	}
}

// A module interface exporting an endpoint of the excluded service promises
// the composition an endpoint this environment does not have.
func TestExcludeServicesRefusesAnInterfaceExportOfIt(t *testing.T) {
	workspace := writeExclusionWorkspaceExporting(t, `    exclude-services:
      - service: shop/cache
`, "interface:\n    endpoints:\n        - service: cache\n          endpoint: tcp\n          visibility: internal\n")
	err := environments.ValidateWorkspace(context.Background(), workspace)
	require.ErrorContains(t, err, "module shop exports shop/cache/tcp through its interface")
}

// An environment that excludes every service of a module leaves the module
// out: its interface exports and its service-entry leave with it, so the
// refusals above do not hold, while a consumer outside the module still does.
func TestExcludeServicesWithdrawsAWholeModuleWithItsExports(t *testing.T) {
	const wholeShop = `    exclude-services:
      - service: shop/api
      - service: shop/vault
      - service: shop/cache
`
	exporting := "service-entry: cache\ninterface:\n    endpoints:\n        - service: cache\n          endpoint: tcp\n          visibility: internal\n"
	workspace := writeExclusionWorkspaceExporting(t, wholeShop, exporting)
	require.NoError(t, environments.ValidateWorkspace(context.Background(), workspace))

	staging, err := environments.Select(workspace, "staging")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(context.Background(), "shop")
	require.NoError(t, err)
	require.Empty(t, staging.ComposedServices(module))
	local, err := environments.Select(workspace, "local")
	require.NoError(t, err)
	require.Len(t, local.ComposedServices(module), 3, "another environment still composes the module")

	// Leaving one service of the module composed keeps its promise in force.
	partial := writeExclusionWorkspaceExporting(t, `    exclude-services:
      - service: shop/vault
        withdrawn-from: [shop/api]
      - service: shop/cache
`, exporting)
	err = environments.ValidateWorkspace(context.Background(), partial)
	require.ErrorContains(t, err, "exclude every service of module shop to leave the module out")

	// A consumer in another module is still a consumer of what left.
	consumed := writeExclusionWorkspaceExporting(t, wholeShop, exporting)
	root := consumed.Dir()
	data, err := os.ReadFile(filepath.Join(root, resources.WorkspaceConfigurationName))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName),
		[]byte(strings.Replace(string(data), "  - name: shop\n", "  - name: shop\n  - name: portal\n", 1)), 0o644))
	for rel, content := range map[string]string{
		"modules/portal/module.codefly.yaml": "kind: module\nname: portal\nservices:\n    - name: web\n",
		"modules/portal/services/web/service.codefly.yaml": `kind: service
name: web
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
service-dependencies:
  - name: cache
    module: shop
    endpoints:
      - name: tcp
`,
	} {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	err = environments.ValidateWorkspace(context.Background(), reloaded)
	require.ErrorContains(t, err, `exclude-services withdraws "shop/cache", but portal/web declares a dependency on it`)
}

// A module naming the excluded service its entry routes the host to a service
// that is not there.
func TestExcludeServicesRefusesTheModuleServiceEntry(t *testing.T) {
	workspace := writeExclusionWorkspaceExporting(t, `    exclude-services:
      - service: shop/cache
`, "service-entry: cache\n")
	err := environments.ValidateWorkspace(context.Background(), workspace)
	require.ErrorContains(t, err, "module shop names shop/cache its service-entry")
}

// A solution consuming one of its endpoints through its manifest is a consumer
// the service graph does not show; it is refused all the same.
func TestExcludeServicesRefusesASolutionConsumingIt(t *testing.T) {
	workspace := writeExclusionWorkspace(t, `    exclude-services:
      - service: shop/cache
`)
	root := workspace.Dir()
	files := map[string]string{
		"workspace.codefly.yaml":                           "",
		"modules/portal/module.codefly.yaml":               "kind: module\nname: portal\nservice-entry: web\nservices:\n    - name: web\n",
		"modules/portal/services/web/service.codefly.yaml": "kind: service\nname: web\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n",
		filepath.Join("modules", "portal", manifest.FileName): `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: portal
  version: 0.1.0
api:
  consumes:
    - id: cache
      protocol: tcp
      module: shop
      service: cache
      endpoint: tcp
      as: cache
`,
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if rel == "workspace.codefly.yaml" {
			data, err := os.ReadFile(full)
			require.NoError(t, err)
			content = strings.Replace(string(data), "  - name: shop\n", "  - name: shop\n  - name: portal\n", 1)
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	err = environments.ValidateWorkspace(context.Background(), reloaded)
	require.ErrorContains(t, err, "solution portal consumes shop/cache/tcp (api.consumes)")
}

// The block's own shape is refused at load, before any graph is read.
func TestExcludeServicesRefusesAMalformedEntry(t *testing.T) {
	for name, tc := range map[string]struct{ declarations, want string }{
		"a bare name": {`    exclude-services:
      - service: vault
`, "must be <module>/<service>"},
		"a repeated entry": {`    exclude-services:
      - service: shop/cache
      - service: shop/cache
`, `names "shop/cache" twice`},
		"an unknown key": {`    exclude-services:
      - service: shop/vault
        withdrawn_from: [shop/api]
`, `"withdrawn_from"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := environments.FromWorkspace(writeExclusionWorkspace(t, tc.declarations))
			require.Error(t, err)
			require.True(t, strings.Contains(err.Error(), tc.want), "error %q does not contain %q", err, tc.want)
		})
	}
}

// The producer's JSON contract preserves the environment exclusion alongside
// the module-owned KMS choice when imported into workspace YAML.
func TestCoordinateContractCarriesExcludedServices(t *testing.T) {
	contract, err := environments.ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","environment":{"name":"blue","namespace":"obin","key-service":"kms","exclude-services":[{"service":"saas/vault","withdrawn-from":["saas/accounts"]}]}}`))
	require.NoError(t, err)
	env, err := contract.ToEnvironment("blue", "obin")
	require.NoError(t, err)
	require.Equal(t, "kms", env.Carried["key-service"].Value)
	require.Equal(t, []environments.EnvironmentExcludedService{{Service: "saas/vault", WithdrawnFrom: []string{"saas/accounts"}}}, env.ExcludeServices)
	env.ExcludeServices[0].WithdrawnFrom[0] = "changed/consumer"
	require.Equal(t, "saas/accounts", contract.Environment.ExcludeServices[0].WithdrawnFrom[0])
}
