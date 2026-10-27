package publish_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

// clientReleaseRepo includes a generated client outside libraries/, a foreign
// client, and an ignored cache. Only a tracked source owned by the releasing
// package moves, and its copied contract bytes remain identical.
func clientReleaseRepo(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir, _, originals := moduleAgentRepo(t, "0.2.0")
	contract := `{"openapi":"3.0.0","paths":{"/ready":{"get":{}}}}`
	digest := composition.APIContractDigest([]byte(contract))
	endpoint := composition.APIContractEndpoint{
		Service: "api", Endpoint: "rest", API: "rest", Kind: composition.APIContractKindOpenAPI,
		Package: "payments", Path: "contracts/api/api/rest/openapi.json", Digest: digest,
		Routes: []composition.APIContractRoute{{Method: "GET", Path: "/ready"}},
	}
	catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "acme/payments", Version: "0.0.0", Endpoints: []composition.APIContractEndpoint{endpoint}}
	data, err := catalog.CanonicalBytes()
	require.NoError(t, err)
	packagePath := "modules/payments/module.package.codefly.yaml"
	originals[packagePath] = strings.Replace(originals[packagePath], "  - name: api\n", "  - name: api\n    endpoints: [rest]\n    api-contracts:\n      - endpoint: rest\n        kind: openapi\n        package: payments\n        path: "+endpoint.Path+"\n        digest: "+digest+"\n", 1)
	originals["modules/payments/"+composition.APIContractCatalogFileName] = string(data)
	originals["modules/payments/"+endpoint.Path] = contract
	catalog.Endpoints[0].Path = "openapi.json"
	data, err = catalog.CanonicalBytes()
	require.NoError(t, err)
	originals["clients/payments/contract/catalog.codefly.json"] = string(data)
	originals["clients/payments/contract/openapi.json"] = contract
	originals["clients/payments/library.codefly.yaml"] = `kind: library
name: payments
version: 0.0.0
languages:
  - name: typescript
    path: typescript/
  - name: python
    path: python/
sources:
  - package: acme/payments
    version: 0.0.0
    module: payments
    service: api
    endpoint: rest
    contract-digest: ` + digest + `
generated-by:
  codefly: 0.1.219
  companion: preserved
  facade: false
`
	originals["clients/payments/typescript/package.json"] = `{"name":"@acme/payments","version":"0.0.0","description":"preserved"}`
	originals["clients/payments/python/pyproject.toml"] = "[project]\nname = \"payments\"\nversion = \"0.0.0\"\n"
	originals["libraries/foreign/library.codefly.yaml"] = "kind: library\nname: foreign\nversion: 1.0.0\nsources:\n  - package: acme/foreign\n    version: 1.0.0\n"
	originals[".gitignore"] = ".cache/\n"
	for relative, content := range originals {
		path := filepath.Join(dir, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".cache"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".cache", "library.codefly.yaml"), []byte("invalid YAML: ["), 0o600))
	require.NoError(t, runGit(dir, "add", "."))
	require.NoError(t, runGit(dir, "commit", "-m", "generated clients"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))
	return dir, originals
}

func TestModuleReleaseStampsTrackedClientsAtOwnerVersion(t *testing.T) {
	dir, originals := clientReleaseRepo(t)
	tag, err := moduleEngine(t, dir, "patch").Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.2.1", tag)
	require.Contains(t, readRel(t, dir, "clients/payments/library.codefly.yaml"), "version: 0.2.1")
	require.NotContains(t, readRel(t, dir, "clients/payments/library.codefly.yaml"), "version: 0.0.0")
	require.Contains(t, readRel(t, dir, "clients/payments/library.codefly.yaml"), "companion: preserved")
	require.Contains(t, readRel(t, dir, "clients/payments/contract/catalog.codefly.json"), `"version": "0.2.1"`)
	require.Equal(t, strings.Replace(originals["clients/payments/typescript/package.json"], `"0.0.0"`, `"0.2.1"`, 1), readRel(t, dir, "clients/payments/typescript/package.json"))
	require.Contains(t, readRel(t, dir, "clients/payments/python/pyproject.toml"), `version = "0.2.1"`)
	for _, path := range []string{"clients/payments/contract/openapi.json", "libraries/foreign/library.codefly.yaml"} {
		require.Equal(t, originals[path], readRel(t, dir, path))
	}
	require.Equal(t, "invalid YAML: [", readRel(t, dir, ".cache/library.codefly.yaml"))
	require.NoError(t, runGit(dir, "diff", "--exit-code", "HEAD"), "every stamped path belongs to the release commit")
}

func TestModuleClientReleaseFailureRestoresEveryStamp(t *testing.T) {
	dir, originals := clientReleaseRepo(t)
	engine := moduleEngine(t, dir, "patch")
	failure := errors.New("release gate rejected candidate")
	engine.BeforeCommit = func(context.Context, string) error {
		require.Contains(t, readRel(t, dir, "clients/payments/contract/catalog.codefly.json"), `"version": "0.2.1"`)
		require.Contains(t, readRel(t, dir, "clients/payments/typescript/package.json"), `"version":"0.2.1"`)
		return failure
	}
	_, err := engine.Release(context.Background())
	require.ErrorIs(t, err, failure)
	for path, before := range originals {
		require.Equal(t, before, readRel(t, dir, path), "%s was not restored", path)
	}
	require.NoError(t, runGit(dir, "diff", "--exit-code", "HEAD"))
}

func TestModuleClientReleaseRefusesEscapingLanguagePath(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "traversal", true: "symlink"}[symlink], func(t *testing.T) {
			dir, originals := clientReleaseRepo(t)
			outside := t.TempDir()
			outsideManifest := filepath.Join(outside, "package.json")
			require.NoError(t, os.WriteFile(outsideManifest, []byte(`{"version":"9.0.0"}`), 0o600))
			if symlink {
				require.NoError(t, os.Remove(filepath.Join(dir, "clients/payments/typescript/package.json")))
				require.NoError(t, os.Symlink(outsideManifest, filepath.Join(dir, "clients/payments/typescript/package.json")))
			} else {
				manifest := strings.Replace(originals["clients/payments/library.codefly.yaml"], "path: typescript/", "path: ../../../../escape", 1)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "clients/payments/library.codefly.yaml"), []byte(manifest), 0o600))
			}
			require.NoError(t, runGit(dir, "add", "."))
			require.NoError(t, runGit(dir, "commit", "-m", "escaping path"))
			require.NoError(t, runGit(dir, "push", "origin", "main"))
			_, err := moduleEngine(t, dir, "patch").Release(context.Background())
			require.ErrorContains(t, err, "escapes")
			bytes, err := os.ReadFile(outsideManifest)
			require.NoError(t, err)
			require.Equal(t, `{"version":"9.0.0"}`, string(bytes))
			require.Equal(t, originals["agent.codefly.yaml"], readRel(t, dir, "agent.codefly.yaml"))
			require.NoError(t, runGit(dir, "diff", "--exit-code", "HEAD"))
		})
	}
}
