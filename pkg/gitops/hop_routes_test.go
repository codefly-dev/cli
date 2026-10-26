package gitops

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/authority/libraries/hop/go/routing"
)

func hopRouteWorkspace(t *testing.T, visibility string) *resources.Workspace {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml":                                "name: routes\nlayout: modules\nmodules:\n - name: platform\n - name: payments\n",
		"configurations/local/hop.env":                          "MODULES=platform\n",
		"modules/platform/module.codefly.yaml":                  "name: platform\nservices:\n - name: first\n - name: second\n",
		"modules/platform/module.contract.codefly.yaml":         "schema: codefly/module-contract/v1\nprincipal: platform\nnamespaces: [platform]\nqueues: []\nscope_ceilings: []\nbindings: []\n",
		"modules/payments/module.codefly.yaml":                  "name: payments\nservices:\n - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "name: worker\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nworkspace-configuration-dependencies: [hop-routes]\n",
	}
	catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "owner/platform", Version: "1.0.0"}
	manifest := "schema: codefly/module-package/v2\nkind: module-package\nid: owner/platform\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n"
	for _, service := range []string{"first", "second"} {
		path := "contracts/api/" + service + ".json"
		doc := fmt.Sprintf(`{"openapi":"3.0.0","info":{"title":"%s","version":"1.0.0"},"paths":{"/%s":{"get":{"responses":{"200":{"description":"ok"}}}}}}`, service, service)
		digest := composition.APIContractDigest([]byte(doc))
		files["modules/platform/"+path] = doc
		files["modules/platform/services/"+service+"/service.codefly.yaml"] = "name: " + service + "\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nendpoints:\n    - name: web\n      api: http\n      visibility: " + visibility + "\n" + "      exposure: none\n"
		manifest += fmt.Sprintf(" - name: %s\n   endpoints: [web]\n   api-contracts:\n    - endpoint: web\n      kind: openapi\n      package: api\n      path: %s\n      digest: %s\n", service, path, digest)
		catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{Service: service, Endpoint: "web", API: "http", Kind: composition.APIContractKindOpenAPI, Package: "api", Path: path, Digest: digest, Routes: []composition.APIContractRoute{{Method: "GET", Path: "/" + service}}})
	}
	raw, err := json.Marshal(catalog)
	require.NoError(t, err)
	files["modules/platform/"+composition.APIContractCatalogFileName] = string(raw)
	files["modules/platform/module.package.codefly.yaml"] = manifest
	root := t.TempDir()
	for rel, raw := range files {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

func TestCellSeesGeneratedHopRoutes(t *testing.T) {
	ctx := context.Background()
	ws := hopRouteWorkspace(t, "public")
	env := selectedEnvironment(t, ws, "local")
	provided, _, err := orchestration.WorkspaceConfigurationsForChecking(ctx, ws, env)
	require.NoError(t, err)
	require.NotEmpty(t, provided.Infos)
	consumers, _, err := endpointConsumers(ctx, ws, env)
	require.NoError(t, err)
	require.Equal(t, []string{"payments/worker"}, consumers["platform/first/web"])
	require.Equal(t, []string{"payments/worker"}, consumers["platform/second/web"])
}

// policyRouteWorkspace is a hop composing platform, whose one listener
// publishes doc while its catalog lists routes: the catalog may disagree with
// its document, which no digest binds.
func policyRouteWorkspace(t *testing.T, doc string, routes []composition.APIContractRoute) *resources.Workspace {
	t.Helper()
	digest := composition.APIContractDigest([]byte(doc))
	catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "owner/platform", Version: "1.0.0", Endpoints: []composition.APIContractEndpoint{
		{Service: "first", Endpoint: "web", API: "http", Kind: composition.APIContractKindOpenAPI, Package: "api", Path: "contracts/api/first.json", Digest: digest, Routes: routes},
	}}
	raw, err := json.Marshal(catalog)
	require.NoError(t, err)
	files := map[string]string{
		"workspace.codefly.yaml":                                     "name: routes\nlayout: modules\nmodules:\n - name: platform\n - name: payments\n",
		"configurations/local/hop.env":                               "MODULES=platform\n",
		"modules/platform/module.codefly.yaml":                       "name: platform\nservices:\n - name: first\n",
		"modules/platform/module.contract.codefly.yaml":              "schema: codefly/module-contract/v1\nprincipal: platform\nnamespaces: [platform]\nqueues: []\nscope_ceilings: []\nbindings: []\n",
		"modules/platform/contracts/api/first.json":                  doc,
		"modules/platform/" + composition.APIContractCatalogFileName: string(raw),
		"modules/platform/services/first/service.codefly.yaml":       "name: first\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nendpoints:\n    - name: web\n      api: http\n      visibility: public\n      exposure: none\n",
		"modules/platform/module.package.codefly.yaml":               "schema: codefly/module-package/v2\nkind: module-package\nid: owner/platform\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n - name: first\n   endpoints: [web]\n   api-contracts:\n    - endpoint: web\n      kind: openapi\n      package: api\n      path: contracts/api/first.json\n      digest: " + digest + "\n",
		"modules/payments/module.codefly.yaml":                       "name: payments\nservices:\n - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml":      "name: worker\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nworkspace-configuration-dependencies: [hop-routes]\n",
	}
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

// hopRoutesOf is the ROUTES snapshot a hop of the workspace is handed.
func hopRoutesOf(t *testing.T, workspace *resources.Workspace) *routing.Routing {
	t.Helper()
	provided, _, err := orchestration.WorkspaceConfigurationsForChecking(context.Background(), workspace, selectedEnvironment(t, workspace, "local"))
	require.NoError(t, err)
	for _, info := range provided.Infos {
		if info.GetName() != "hop-routes" {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			if value.GetKey() == "ROUTES" {
				routes, err := routing.DecodeRouting(value.GetValue())
				require.NoError(t, err)
				return routes
			}
		}
	}
	t.Fatal("no hop-routes/ROUTES generated")
	return nil
}

// The hop maps a call onto a route the authority holds a policy for, read
// from the one table delivered to it: a catalog that omits a stricter route
// its document declares cannot make the hop check that call against a looser
// template. Every listener's ROUTES methods are its delivered policy methods,
// basePath included.
func TestHopRoutesAreTheDeliveredPolicyTable(t *testing.T) {
	const policies = `"/v1/things/{id}":{"get":{"x-codefly-authority-policy":{"anyCheckedCaller":true},"responses":{"200":{"description":"ok"}}}},` +
		`"/v1/things/export":{"get":{"x-codefly-authority-policy":{"requires":[{"resourceKind":"thing","actions":["export"]}]},"responses":{"200":{"description":"ok"}}}}`
	for _, tc := range []struct {
		name, doc, call, template string
	}{
		{"openapi 3", `{"openapi":"3.0.0","info":{"title":"api","version":"1.0.0"},"paths":{` + policies + `}}`, "GET /v1/things/export", "GET /v1/things/export"},
		{"swagger basePath", `{"swagger":"2.0","info":{"title":"api","version":"1.0.0"},"basePath":"/api","paths":{` + policies + `}}`, "GET /api/v1/things/export", "GET /api/v1/things/export"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The catalog lists only the looser route, with the right digest.
			workspace := policyRouteWorkspace(t, tc.doc, []composition.APIContractRoute{{Method: "GET", Path: "/v1/things/{id}"}})
			routes := hopRoutesOf(t, workspace)
			_, template, err := routes.Resolve("platform", "http", tc.call)
			require.NoError(t, err)
			require.Equal(t, tc.template, template, "the hop checks the stricter route's own policy")

			module, err := workspace.LoadModuleFromName(context.Background(), "platform")
			require.NoError(t, err)
			delivered, ok, err := MethodPolicies(module.Dir())
			require.NoError(t, err)
			require.True(t, ok)
			var want []string
			for _, m := range delivered {
				want = append(want, m.GetMethod())
			}
			require.Len(t, routes.Endpoints, 1)
			require.Equal(t, want, routes.Endpoints[0].Methods, "ROUTES is the delivered table, spelled the same way")
			for _, m := range delivered {
				if strings.HasSuffix(m.GetMethod(), "/export") {
					require.Equal(t, "thing", m.GetRequires()[0].GetResourceKind())
				}
			}
		})
	}
}
