package publish

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/cli/pkg/librarystore"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func accountsPlan(digest string) *generators.ModuleClientPlan {
	return &generators.ModuleClientPlan{
		Name: "saas-starter-accounts-connect-client",
		Endpoint: composition.APIContractEndpoint{
			Service: "accounts", Endpoint: "connect", Kind: composition.APIContractKindProtobuf,
			Package: "saas.accounts.v1", Digest: digest,
		},
		Languages: []languages.Language{languages.GO, languages.TYPESCRIPT},
		Services:  []string{"AuditService"},
	}
}

var clientsTestStoreConfig = librarystore.StoreConfig{
	GoOwner: "codefly-dev", NpmRegistry: "https://npm.pkg.github.com", NpmScope: "@codefly-dev", PythonOwner: "codefly-dev",
}

func recordedExport(t *testing.T, plan *generators.ModuleClientPlan, language languages.Language) *PublishedClientExport {
	t.Helper()
	path, hint, err := librarystore.PreviewIdentity(librarystore.Language(language), clientsTestStoreConfig, plan.Name, "0.1.0")
	require.NoError(t, err)
	return &PublishedClientExport{Language: string(language), ImportPath: path, Ref: "immutable-ref", Digest: "sha256:abcd", InstallHint: hint}
}

func TestClientsCommandReturnsErrorsThroughCobra(t *testing.T) {
	if clientsCmd.RunE == nil || clientsCmd.Run != nil {
		t.Fatal("publish clients command is not exclusively RunE")
	}
	require.NoError(t, clientsCmd.Args(clientsCmd, nil), "the module defaults to the active one")
	require.NoError(t, clientsCmd.Args(clientsCmd, []string{"saas-starter"}))
	require.Error(t, clientsCmd.Args(clientsCmd, []string{"a", "b"}))
}

func TestLoadClientsManifestMissingFileIsEmpty(t *testing.T) {
	manifest, err := LoadClientsManifest(t.TempDir())
	require.NoError(t, err)
	require.Equal(t, ClientsManifestSchema, manifest.Schema)
	require.Empty(t, manifest.Clients)
}

func TestLoadClientsManifestRejectsUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "contracts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contracts", ClientsManifestFileName), []byte(`{"schema":"codefly/module-clients/v9"}`), 0o644))
	_, err := LoadClientsManifest(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "v9")
}

func TestLoadClientsManifestRejectsIncompleteEvidence(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "contracts"), 0o755))
	data := `{"schema":"codefly/module-clients/v1","package":"codefly/saas-starter","version":"0.1.0","clients":[{"library":"client","service":"accounts","endpoint":"connect","contractDigest":"sha256:aaaa","exports":[{"language":"go"}]}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contracts", ClientsManifestFileName), []byte(data), 0o644))
	_, err := LoadClientsManifest(dir)
	require.ErrorContains(t, err, "incomplete publication evidence")
}

func TestClientsManifestForVersionDropsAnotherVersionsRecords(t *testing.T) {
	stored := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0",
		Clients: []PublishedClient{{Library: "saas-starter-accounts-client"}}}
	same := stored.forVersion("codefly/saas-starter", "0.1.0")
	require.Len(t, same.Clients, 1)
	bumped := stored.forVersion("codefly/saas-starter", "0.1.1")
	require.Empty(t, bumped.Clients)
	require.Equal(t, "0.1.1", bumped.Version)
	require.Equal(t, "codefly/saas-starter", bumped.Package)
}

func TestValidateClientPackageRejectsArtifactAndVersionDrift(t *testing.T) {
	dir := t.TempDir()
	contractPath := filepath.Join(dir, "contracts", "api", "accounts", "connect", "contract.binpb")
	require.NoError(t, os.MkdirAll(filepath.Dir(contractPath), 0o755))
	contract := []byte("committed contract")
	require.NoError(t, os.WriteFile(contractPath, contract, 0o644))
	digest := composition.APIContractDigest(contract)
	manifest := fmt.Sprintf(`kind: module-package
schema: codefly/module-package/v2
id: codefly/saas-starter
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots: [contracts/api]
contracts: {composition: ">=2.0"}
services:
  - name: accounts
    endpoints: [connect]
    api-contracts:
      - endpoint: connect
        kind: protobuf
        package: saas.accounts.v1
        path: contracts/api/accounts/connect/contract.binpb
        digest: %s
`, digest)
	require.NoError(t, os.WriteFile(filepath.Join(dir, composition.PackageManifestFileName), []byte(manifest), 0o644))
	catalog := &composition.APIContractCatalog{
		Schema: composition.APIContractCatalogSchema, Package: "codefly/saas-starter", Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service: "accounts", Endpoint: "connect", API: "connect", Kind: composition.APIContractKindProtobuf,
			Package: "saas.accounts.v1", Path: "contracts/api/accounts/connect/contract.binpb", Digest: digest,
			Services: []composition.APIContractService{{Name: "AccountsService", FullName: "saas.accounts.v1.AccountsService"}},
		}},
	}
	require.NoError(t, validateClientPackage(dir, "saas-starter", catalog))

	require.NoError(t, os.WriteFile(contractPath, []byte("corrupt"), 0o644))
	require.ErrorContains(t, validateClientPackage(dir, "saas-starter", catalog), "digest")
	require.NoError(t, os.WriteFile(contractPath, contract, 0o644))
	catalog.Version = "0.1.1"
	require.ErrorContains(t, validateClientPackage(dir, "saas-starter", catalog), "identifies codefly/saas-starter@0.1.0")
}

func TestReconcileClientExportDecisions(t *testing.T) {
	manifest := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0"}
	plan := accountsPlan("sha256:aaaa")

	goExport := recordedExport(t, plan, languages.GO)
	action, err := reconcileClientExport(manifest, plan, languages.GO, goExport.ImportPath, goExport.InstallHint)
	require.NoError(t, err)
	require.Equal(t, actionPublish, action, "nothing recorded: publish")

	manifest.record(plan, goExport)

	action, err = reconcileClientExport(manifest, plan, languages.GO, goExport.ImportPath, goExport.InstallHint)
	require.NoError(t, err)
	require.Equal(t, actionSkip, action, "recorded at the same digest: skip")

	tsExport := recordedExport(t, plan, languages.TYPESCRIPT)
	action, err = reconcileClientExport(manifest, plan, languages.TYPESCRIPT, tsExport.ImportPath, tsExport.InstallHint)
	require.NoError(t, err)
	require.Equal(t, actionPublish, action, "a sibling language not yet recorded still publishes")

	_, err = reconcileClientExport(manifest, accountsPlan("sha256:bbbb"), languages.TYPESCRIPT, tsExport.ImportPath, tsExport.InstallHint)
	require.ErrorIs(t, err, errContractMoved)
	require.Contains(t, err.Error(), "sha256:aaaa")
	require.Contains(t, err.Error(), "sha256:bbbb")
	require.Contains(t, err.Error(), composition.PackageManifestFileName)
}

func TestReconcileClientExportRejectsChangedFacadeAndIncompleteEvidence(t *testing.T) {
	manifest := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0"}
	plan := accountsPlan("sha256:aaaa")
	export := recordedExport(t, plan, languages.GO)
	manifest.record(plan, export)

	changed := accountsPlan("sha256:aaaa")
	changed.Services = []string{"WebhookService"}
	_, err := reconcileClientExport(manifest, changed, languages.GO, export.ImportPath, export.InstallHint)
	require.ErrorIs(t, err, errContractMoved)
	require.ErrorContains(t, err, "facade services")

	manifest.client(plan.Name).Exports[0].Digest = ""
	_, err = reconcileClientExport(manifest, plan, languages.GO, export.ImportPath, export.InstallHint)
	require.ErrorContains(t, err, "incomplete")
}

func TestClientsManifestRoundTripIsCanonical(t *testing.T) {
	dir := t.TempDir()
	manifest := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0"}
	gateway := &generators.ModuleClientPlan{Name: "saas-starter-auth-gateway-grpc-client",
		Endpoint: composition.APIContractEndpoint{Service: "auth-gateway", Endpoint: "grpc", Digest: "sha256:cccc"}}
	manifest.record(gateway, recordedExport(t, gateway, languages.GO))
	accountsPlan := accountsPlan("sha256:aaaa")
	manifest.record(accountsPlan, recordedExport(t, accountsPlan, languages.TYPESCRIPT))
	oldGo := recordedExport(t, accountsPlan, languages.GO)
	oldGo.ImportPath = "go-old"
	manifest.record(accountsPlan, oldGo)
	newGo := recordedExport(t, accountsPlan, languages.GO)
	newGo.ImportPath = "go-new"
	manifest.record(accountsPlan, newGo)
	require.NoError(t, manifest.Save(t.Context(), dir))

	loaded, err := LoadClientsManifest(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"saas-starter-accounts-connect-client", "saas-starter-auth-gateway-grpc-client"},
		[]string{loaded.Clients[0].Library, loaded.Clients[1].Library}, "clients sorted by library")
	accounts := loaded.Clients[0]
	require.Equal(t, []string{"AuditService"}, accounts.Services)
	require.Equal(t, "go", accounts.Exports[0].Language, "exports sorted by language")
	require.Equal(t, "go-new", accounts.Exports[0].ImportPath, "re-recording a language replaces it")
	require.Equal(t, "typescript", accounts.Exports[1].Language)

	first, err := os.ReadFile(filepath.Join(dir, "contracts", ClientsManifestFileName))
	require.NoError(t, err)
	require.NoError(t, loaded.Save(t.Context(), dir))
	second, err := os.ReadFile(filepath.Join(dir, "contracts", ClientsManifestFileName))
	require.NoError(t, err)
	require.Equal(t, string(first), string(second), "save is idempotent")
}

func TestCheckClientsNamesEveryMissingOrStaleExport(t *testing.T) {
	manifest := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0"}
	plan := accountsPlan("sha256:aaaa")
	manifest.record(plan, recordedExport(t, plan, languages.GO))

	var out bytes.Buffer
	version := semver.MustParse("0.1.0")
	err := checkClients(&out, manifest, []generators.ModuleClientPlan{*plan}, version, clientsTestStoreConfig)
	require.Error(t, err)
	require.Contains(t, err.Error(), "saas-starter-accounts-connect-client typescript is not published at 0.1.0")
	require.NotContains(t, err.Error(), "saas-starter-accounts-connect-client go is not")

	manifest.record(plan, recordedExport(t, plan, languages.TYPESCRIPT))
	out.Reset()
	require.NoError(t, checkClients(&out, manifest, []generators.ModuleClientPlan{*plan}, version, clientsTestStoreConfig))
	require.Contains(t, out.String(), "codefly/saas-starter@0.1.0")

	err = checkClients(&out, manifest, []generators.ModuleClientPlan{*accountsPlan("sha256:bbbb")}, version, clientsTestStoreConfig)
	require.Error(t, err)
	require.Contains(t, err.Error(), errContractMoved.Error())
	require.Contains(t, err.Error(), "bump the version")
}

func TestWithClientsPublishLockPreventsLostManifestUpdates(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	moduleDir := t.TempDir()
	base := accountsPlan("sha256:aaaa")
	other := accountsPlan("sha256:bbbb")
	other.Name = "saas-starter-billing-grpc-client"
	other.Endpoint.Service = "billing"
	other.Endpoint.Endpoint = "grpc"

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, plan := range []*generators.ModuleClientPlan{base, other} {
		plan := plan
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- withClientsPublishLock(moduleDir, func() error {
				manifest, err := LoadClientsManifest(moduleDir)
				if err != nil {
					return err
				}
				manifest = manifest.forVersion("codefly/saas-starter", "0.1.0")
				time.Sleep(25 * time.Millisecond)
				manifest.record(plan, &PublishedClientExport{Language: "go", ImportPath: "path", Ref: "ref", Digest: "sha256:d", InstallHint: "hint"})
				return manifest.Save(t.Context(), moduleDir)
			})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	manifest, err := LoadClientsManifest(moduleDir)
	require.NoError(t, err)
	require.Len(t, manifest.Clients, 2)
}

func TestPublishCheckpointsEachExportBeforeStartingTheNext(t *testing.T) {
	exports := []*resources.LanguageExport{{Name: "go"}, {Name: "typescript"}}
	checkpointed := map[string]bool{}
	err := publishAndCheckpointClientExports(exports, func(export *resources.LanguageExport) (publishedExport, error) {
		if export.Name == "typescript" {
			require.True(t, checkpointed["go"], "go must be durable before the next irreversible publish starts")
		}
		return publishedExport{Language: librarystore.Language(export.Name)}, nil
	}, func(p publishedExport) error {
		checkpointed[string(p.Language)] = true
		return nil
	})
	require.NoError(t, err)
	require.True(t, checkpointed["typescript"])
}
