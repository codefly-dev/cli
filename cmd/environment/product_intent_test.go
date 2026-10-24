package environment

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

const productExclusionIntent = `environments:
  azure:
    exclude-services:
      - service: shop/vault
        withdrawn-from: [shop/api]
  green:
    exclude-services: []
`

func productImportWorkspace(t *testing.T) string {
	t.Helper()
	dir := writeWorkspace(t, "name: acme\nlayout: modules\nmodules:\n  - name: shop\nenvironments:\n  - name: local # keep local\n")
	files := map[string]string{
		"modules/shop/module.codefly.yaml": "kind: module\nname: shop\nservices:\n  - name: api\n  - name: vault\n  - name: cache\n",
	}
	for _, name := range []string{"api", "vault", "cache"} {
		service := "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n"
		if name == "api" {
			service += "service-dependencies:\n  - name: vault\n"
		}
		files["modules/shop/services/"+name+"/service.codefly.yaml"] = service
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return dir
}

func TestProductIntentImportPreservesPlacementAndSupportsClearing(t *testing.T) {
	dir := productImportWorkspace(t)
	contract := strings.Replace(fixtureContract, `"name": "azure",`, `"name": "azure", "key-service": "kms",`, 1)
	doImport(t, dir, importOptions{contractData: []byte(contract), productIntentData: []byte(productExclusionIntent)})
	env := loadWorkspace(t, dir).FindEnvironment("azure")
	require.True(t, env.Excludes("shop", "vault"))
	require.Equal(t, []string{"shop/api"}, env.ExcludeServices[0].WithdrawnFrom)
	require.Equal(t, "kms", env.Carried["key-service"].Value)
	require.Equal(t, "x.azurecr.io", env.Registry.URL)
	require.Equal(t, "acme", env.Namespace)
	require.False(t, loadWorkspace(t, dir).FindEnvironment("local").Excludes("shop", "vault"))
	require.Nil(t, loadWorkspace(t, dir).FindEnvironment("green"))
	require.Contains(t, readFile(t, filepath.Join(dir, resources.WorkspaceConfigurationName)), "# keep local")

	// Producer-only refreshes do not erase product-owned composition decisions.
	doImport(t, dir, importOptions{})
	require.True(t, loadWorkspace(t, dir).FindEnvironment("azure").Excludes("shop", "vault"))
	// An authored empty list explicitly withdraws the exclusion on re-import.
	doImport(t, dir, importOptions{productIntentData: []byte("environments:\n  azure:\n    exclude-services: []\n")})
	require.Empty(t, loadWorkspace(t, dir).FindEnvironment("azure").ExcludeServices)
}

func TestProductIntentRefusalsDoNotWrite(t *testing.T) {
	for name, intent := range map[string]string{
		"placement override":     strings.Replace(productExclusionIntent, "  azure:\n", "  azure:\n    namespace: elsewhere\n", 1),
		"producer key selector":  strings.Replace(productExclusionIntent, "  azure:\n", "  azure:\n    key-service: vault\n", 1),
		"unknown top key":        "registry: attacker.example\n" + productExclusionIntent,
		"legacy launcher":        "name: product\nconfiguration-profiles: [local]\n" + productExclusionIntent,
		"unknown exclusion key":  strings.Replace(productExclusionIntent, "withdrawn-from:", "withdrawn-fom:", 1),
		"missing environment":    strings.Replace(productExclusionIntent, "azure:", "blue:", 1),
		"omitted exclusion":      "environments:\n  azure: {}\n",
		"null exclusion":         "environments:\n  azure:\n    exclude-services: null\n",
		"multiple documents":     productExclusionIntent + "---\nenvironments: {}\n",
		"bare service":           strings.Replace(productExclusionIntent, "shop/vault", "vault", 1),
		"unknown service":        strings.Replace(productExclusionIntent, "shop/vault", "shop/missing", 1),
		"nonconsumer withdrawal": strings.Replace(productExclusionIntent, "shop/api", "shop/cache", 1),
		"missing withdrawal":     strings.Replace(productExclusionIntent, "        withdrawn-from: [shop/api]\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir := productImportWorkspace(t)
			file := filepath.Join(dir, resources.WorkspaceConfigurationName)
			before := readFile(t, file)
			err := runImport(context.Background(), &importOptions{dir: dir, envName: "azure", contractData: []byte(fixtureContract), productIntentData: []byte(intent), stdout: io.Discard})
			require.Error(t, err)
			require.Equal(t, before, readFile(t, file))
		})
	}
}

func TestProductIntentRefusesDualOwnership(t *testing.T) {
	dir := productImportWorkspace(t)
	contract := strings.Replace(fixtureContract, `"name": "azure",`, `"name": "azure", "exclude-services": [],`, 1)
	err := runImport(t.Context(), &importOptions{dir: dir, envName: "azure", contractData: []byte(contract), productIntentData: []byte(productExclusionIntent), stdout: io.Discard})
	require.ErrorContains(t, err, "both coordinate contract and product intent")
}

func TestProductIntentFlagDryRun(t *testing.T) {
	dir := productImportWorkspace(t)
	path := filepath.Join(dir, "delivery.environments.yaml")
	require.NoError(t, os.WriteFile(path, []byte(productExclusionIntent), 0o644))
	file := filepath.Join(dir, resources.WorkspaceConfigurationName)
	before := readFile(t, file)
	t.Cleanup(func() {
		require.NoError(t, importCmd.Flags().Set("product-intent", ""))
		importCmd.Flags().Lookup("product-intent").Changed = false
	})
	require.NoError(t, executeImport(t, dir, "--product-intent", path, "--dry-run"))
	require.Equal(t, before, readFile(t, file))
}
