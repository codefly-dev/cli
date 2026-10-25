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
	"testing"
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
