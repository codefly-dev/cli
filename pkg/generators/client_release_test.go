package generators

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func releaseClientEntry(module, version string) ContractEntry {
	data := []byte(`{"openapi":"3.0.0","paths":{"/ready":{"get":{}}}}`)
	return ContractEntry{
		PackageID: "acme/" + module, PackageVersion: version, ModuleName: module, ContractBytes: data,
		Endpoint: composition.APIContractEndpoint{
			Service: "gateway", Endpoint: "rest", API: "rest", Kind: composition.APIContractKindOpenAPI,
			Package: module, Path: "contracts/api/gateway/rest/openapi.json", Digest: composition.APIContractDigest(data),
			Routes: []composition.APIContractRoute{{Method: "GET", Path: "/ready"}},
		},
	}
}

func releaseClientFixture(t *testing.T, entries []ContractEntry) (string, []generatedLanguageExport) {
	t.Helper()
	dir := t.TempDir()
	langs := []generatedLanguageExport{{Name: "typescript", Path: "typescript/"}, {Name: "python", Path: "python/"}, {Name: "go", Path: "go/"}}
	require.NoError(t, writeContractFiles(t.Context(), dir, entries))
	for _, entry := range entries {
		path := filepath.Join(dir, "producer", entry.Endpoint.Path)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, entry.ContractBytes, 0o600))
	}
	for _, lang := range langs {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, lang.Path), 0o755))
	}
	require.NoError(t, writeTypeScriptScaffold(filepath.Join(dir, "typescript"), "@acme/client"))
	require.NoError(t, writePythonScaffold(filepath.Join(dir, "python"), "acme_client"))
	require.NoError(t, writeLibraryManifest(t.Context(), dir, "client", "0.1.0", entries, langs, false))
	return dir, langs
}

func TestClientReleaseMatchesGeneratorVersionOutputs(t *testing.T) {
	entry := releaseClientEntry("payments", "0.1.0")
	dir, langs := releaseClientFixture(t, []ContractEntry{entry})
	owner := &composition.APIContractCatalog{Package: entry.PackageID, Version: entry.PackageVersion, Endpoints: []composition.APIContractEndpoint{entry.Endpoint}}
	plan, err := PlanClientRelease(dir, map[string]ClientReleasePackage{entry.PackageID: {Directory: filepath.Join(dir, "producer"), Catalog: owner}}, func(string) error { return nil })
	require.NoError(t, err)
	require.Len(t, plan.Paths, 4)
	require.NoError(t, plan.Write(t.Context(), map[string]string{entry.PackageID: "0.2.0"}))
	after := snapshotTree(t, dir)

	// Re-run the same version-writing paths as GenerateClientLibrary: nothing
	// differs from the release output, including its copied contract catalog.
	entry.PackageVersion = "0.2.0"
	require.NoError(t, writeContractFiles(t.Context(), dir, []ContractEntry{entry}))
	require.NoError(t, writeLibraryManifest(t.Context(), dir, "client", libraryVersion(dir, []ContractEntry{entry}), []ContractEntry{entry}, langs, false))
	for _, lang := range langs {
		require.NoError(t, stampPackageVersion(languages.Language(lang.Name), filepath.Join(dir, lang.Path), "0.2.0"))
	}
	require.Equal(t, after, snapshotTree(t, dir))
}

func TestClientReleaseAggregateKeepsForeignSourceAndOwnVersion(t *testing.T) {
	entries := []ContractEntry{releaseClientEntry("payments", "0.1.0"), releaseClientEntry("ledger", "3.4.0")}
	dir, _ := releaseClientFixture(t, entries)
	foreignPath := filepath.Join(dir, "contract", "ledger-gateway-rest", "catalog.codefly.json")
	foreign, err := os.ReadFile(foreignPath)
	require.NoError(t, err)
	ts, err := os.ReadFile(filepath.Join(dir, "typescript", "package.json"))
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, resources.LibraryConfigurationName)
	manifest, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, append(manifest, []byte("custom-owner: keep\n")...), 0o600))

	owner := &composition.APIContractCatalog{Package: "acme/payments", Version: "0.1.0", Endpoints: []composition.APIContractEndpoint{entries[0].Endpoint}}
	plan, err := PlanClientRelease(dir, map[string]ClientReleasePackage{"acme/payments": {Directory: filepath.Join(dir, "producer"), Catalog: owner}}, func(string) error { return nil })
	require.NoError(t, err)
	require.Len(t, plan.Paths, 2)
	require.NoError(t, plan.Write(t.Context(), map[string]string{"acme/payments": "0.2.0"}))
	got, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(got, &doc))
	require.Equal(t, "0.1.0", doc["version"])
	require.Equal(t, "keep", doc["custom-owner"])
	var original map[string]any
	require.NoError(t, yaml.Unmarshal(manifest, &original))
	require.Equal(t, original["generated-by"], doc["generated-by"])
	sources, err := readExistingLibrarySources(dir)
	require.NoError(t, err)
	require.Equal(t, "0.2.0", sources[0].Version)
	require.Equal(t, "3.4.0", sources[1].Version)
	got, err = os.ReadFile(foreignPath)
	require.NoError(t, err)
	require.Equal(t, foreign, got)
	got, err = os.ReadFile(filepath.Join(dir, "typescript", "package.json"))
	require.NoError(t, err)
	require.Equal(t, ts, got)
}

func TestClientReleaseRefusesStaleArtifactsBeforeWriting(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*testing.T, string, *composition.APIContractCatalog)
		want   string
	}{
		{"producer changed", func(_ *testing.T, _ string, owner *composition.APIContractCatalog) {
			owner.Endpoints[0].Digest = composition.APIContractDigest([]byte("changed"))
		}, "is stale"},
		{"producer version changed", func(_ *testing.T, _ string, owner *composition.APIContractCatalog) { owner.Version = "0.2.0" }, "is stale"},
		{"copied contract changed", func(t *testing.T, dir string, _ *composition.APIContractCatalog) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "contract", "openapi.json"), []byte("changed"), 0o600))
		}, "does not match catalog digest"},
		{"unknown catalog field", func(t *testing.T, dir string, _ *composition.APIContractCatalog) {
			path := filepath.Join(dir, "contract", "catalog.codefly.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append([]byte(`{"unknown":true,`), data[1:]...), 0o600))
		}, "unknown field"},
		{"language version absent", func(t *testing.T, dir string, _ *composition.APIContractCatalog) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "typescript", "package.json"), []byte(`{"name":"client"}`), 0o600))
		}, "cannot find a version"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			entry := releaseClientEntry("payments", "0.1.0")
			dir, _ := releaseClientFixture(t, []ContractEntry{entry})
			owner := &composition.APIContractCatalog{Package: entry.PackageID, Version: entry.PackageVersion, Endpoints: []composition.APIContractEndpoint{entry.Endpoint}}
			scenario.mutate(t, dir, owner)
			before := snapshotTree(t, dir)
			_, err := PlanClientRelease(dir, map[string]ClientReleasePackage{entry.PackageID: {Directory: filepath.Join(dir, "producer"), Catalog: owner}}, func(string) error { return nil })
			require.ErrorContains(t, err, scenario.want)
			require.Equal(t, before, snapshotTree(t, dir))
		})
	}
}

func TestClientReleaseDoesNotReadForeignCatalogs(t *testing.T) {
	dir, _ := releaseClientFixture(t, []ContractEntry{releaseClientEntry("ledger", "1.0.0")})
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "contract")))
	plan, err := PlanClientRelease(dir, map[string]ClientReleasePackage{"acme/payments": {}}, func(string) error { return nil })
	require.NoError(t, err)
	require.Nil(t, plan)
}

func TestClientReleaseRefusesSelfConsistentForeignContract(t *testing.T) {
	entry := releaseClientEntry("payments", "0.1.0")
	dir, _ := releaseClientFixture(t, []ContractEntry{entry})
	owner := &composition.APIContractCatalog{Package: entry.PackageID, Version: entry.PackageVersion, Endpoints: []composition.APIContractEndpoint{entry.Endpoint}}
	copiedPath := filepath.Join(dir, "contract", "catalog.codefly.json")
	copied, err := readClientReleaseCatalog(copiedPath)
	require.NoError(t, err)
	changed := []byte(`{"openapi":"3.0.0","paths":{"/different":{"get":{}}}}`)
	copied.Endpoints[0].Digest = composition.APIContractDigest(changed)
	copied.Endpoints[0].Routes[0].Path = "/different"
	data, err := copied.CanonicalBytes()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(copiedPath, data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contract", "openapi.json"), changed, 0o600))
	before := snapshotTree(t, dir)
	_, err = PlanClientRelease(dir, map[string]ClientReleasePackage{entry.PackageID: {Directory: filepath.Join(dir, "producer"), Catalog: owner}}, func(string) error { return nil })
	require.ErrorContains(t, err, "differs from its producer")
	require.Equal(t, before, snapshotTree(t, dir))
}

func TestClientReleaseVerifiesNarrowedProducerContract(t *testing.T) {
	entry := narrowEntry(narrowFixture(t), "acme.v1.Wanted")
	entry.PackageID, entry.PackageVersion = "acme/payments", "0.1.0"
	entry.Endpoint.API, entry.Endpoint.Path = "connect", "contracts/api/api/connect/contract.binpb"
	owner := &composition.APIContractCatalog{Package: entry.PackageID, Version: entry.PackageVersion, Endpoints: []composition.APIContractEndpoint{entry.Endpoint}}
	narrowed, err := narrowEntries([]ContractEntry{entry})
	require.NoError(t, err)
	dir, _ := releaseClientFixture(t, narrowed)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "producer", entry.Endpoint.Path), entry.ContractBytes, 0o600))
	packages := map[string]ClientReleasePackage{entry.PackageID: {Directory: filepath.Join(dir, "producer"), Catalog: owner}}
	plan, err := PlanClientRelease(dir, packages, func(string) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, plan)
	// A different valid subset cannot be certified under the saved selection.
	other := entry
	other.Services = []string{"acme.v1.Other"}
	otherEntries, err := narrowEntries([]ContractEntry{other})
	require.NoError(t, err)
	require.NoError(t, writeContractFiles(t.Context(), dir, otherEntries))
	_, err = PlanClientRelease(dir, packages, func(string) error { return nil })
	require.ErrorContains(t, err, "service selection disagrees")
}

func TestClientReleaseSamePackageAggregateKeepsAggregateVersion(t *testing.T) {
	entries := []ContractEntry{releaseClientEntry("payments", "0.1.0"), releaseClientEntry("payments", "0.1.0")}
	entries[1].Endpoint.Service = "worker"
	entries[1].Endpoint.Path = "contracts/api/worker/rest/openapi.json"
	dir, _ := releaseClientFixture(t, entries)
	owner := &composition.APIContractCatalog{Package: "acme/payments", Version: "0.1.0", Endpoints: []composition.APIContractEndpoint{entries[0].Endpoint, entries[1].Endpoint}}
	plan, err := PlanClientRelease(dir, map[string]ClientReleasePackage{"acme/payments": {Directory: filepath.Join(dir, "producer"), Catalog: owner}}, func(string) error { return nil })
	require.NoError(t, err)
	require.Len(t, plan.Paths, 3)
	require.NoError(t, plan.Write(t.Context(), map[string]string{"acme/payments": "0.2.0"}))
	require.Equal(t, "0.1.0", existingTopVersion(dir))
	sources, err := readExistingLibrarySources(dir)
	require.NoError(t, err)
	for _, source := range sources {
		require.Equal(t, "0.2.0", source.Version)
	}
}
