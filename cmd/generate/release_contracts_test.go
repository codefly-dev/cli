package generate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// releaseFixture is a packageable module at 0.1.0 whose catalog, package
// manifest and derived runnables are all current, the state a release starts
// from.
func releaseFixture(t *testing.T) (root, moduleDir string) {
	t.Helper()
	ctx := context.Background()
	root, moduleDir = saveConnectRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	catalog, err := composition.LoadAPIContractCatalog(moduleDir)
	require.NoError(t, err)
	endpoint := catalog.Endpoints[0]
	writeFixtureFile(t, filepath.Join(moduleDir, composition.PackageManifestFileName), []byte(
		"kind: module-package\nschema: codefly/module-package/v2\nid: codefly/documents\nversion: 0.1.0\n"+
			"minimum-codefly-version: \">=0.1.0\"\ncontracts:\n  composition: \">=2.0 <3.0\"\nartifact-roots:\n  - contracts\n"+
			"services:\n  - name: runtime-worker\n    endpoints: [grpc, connect]\n    api-contracts:\n"+
			"      - endpoint: "+endpoint.Endpoint+"\n        kind: "+endpoint.Kind+"\n        package: "+endpoint.Package+
			"\n        path: "+endpoint.Path+"\n        digest: "+endpoint.Digest+"\n"))
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	return root, moduleDir
}

func setPackageVersion(t *testing.T, moduleDir, version string) {
	t.Helper()
	path := filepath.Join(moduleDir, composition.PackageManifestFileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(data), "version: 0.1.0\n", "version: "+version+"\n", 1)), 0o600))
}

func rekey(t *testing.T, root, moduleDir string) error {
	t.Helper()
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := resources.LoadModuleFromDir(ctx, moduleDir)
	require.NoError(t, err)
	return RekeyReleasedContracts(ctx, workspace.Name, workspace.Dir(), module)
}

// A release that moves the package version re-keys the catalog and every
// derived operation to it, and what it writes is what the generators would:
// `generate runnables --check` finds the tree current afterwards.
func TestRekeyReleasedContractsCarriesThePackageVersion(t *testing.T) {
	root, moduleDir := releaseFixture(t)
	paths, err := ReleasedContractPaths(moduleDir)
	require.NoError(t, err)
	require.Equal(t, []string{
		filepath.Join(moduleDir, filepath.FromSlash(composition.APIContractCatalogFileName)),
		filepath.Join(moduleDir, filepath.FromSlash(runnablespkg.DerivedDir)),
	}, paths)

	setPackageVersion(t, moduleDir, "0.2.0")
	runnablesCheck = true
	require.Error(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}), "moving the package version alone leaves the derived tree stale")
	runnablesCheck = false

	require.NoError(t, rekey(t, root, moduleDir))
	catalog, err := composition.LoadAPIContractCatalog(moduleDir)
	require.NoError(t, err)
	require.Equal(t, "0.2.0", catalog.Version)
	index, err := runnablespkg.LoadIndex(moduleDir)
	require.NoError(t, err)
	require.NotEmpty(t, index.Operations)
	for _, operation := range index.Operations {
		require.Equal(t, "0.2.0", operation.Version)
	}
	runnablesCheck = true
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}), "the re-keyed tree is the one generation writes")
}

// A catalog that no longer matches its contract files is refused, not
// re-stamped with a release version, and nothing is written.
func TestRekeyReleasedContractsRefusesStaleContracts(t *testing.T) {
	root, moduleDir := releaseFixture(t)
	catalogPath := filepath.Join(moduleDir, filepath.FromSlash(composition.APIContractCatalogFileName))
	before, err := os.ReadFile(catalogPath)
	require.NoError(t, err)
	writeFixtureFile(t, filepath.Join(moduleDir, "contracts/api/runtime-worker/grpc/contract.binpb"), []byte("moved"))
	setPackageVersion(t, moduleDir, "0.2.0")

	err = rekey(t, root, moduleDir)
	require.ErrorContains(t, err, "the committed contracts are stale")
	require.ErrorContains(t, err, "codefly generate contracts documents")
	after, err := os.ReadFile(catalogPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// A module with no package manifest has no release version to carry.
func TestReleasedContractPathsIgnoresAModuleWithNoPackage(t *testing.T) {
	root, moduleDir := releaseFixture(t)
	require.NoError(t, os.Remove(filepath.Join(moduleDir, composition.PackageManifestFileName)))
	paths, err := ReleasedContractPaths(moduleDir)
	require.NoError(t, err)
	require.Empty(t, paths)
	require.NoError(t, rekey(t, root, moduleDir))
}
