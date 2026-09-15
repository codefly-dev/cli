package publish

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/stretchr/testify/require"
)

func accountsPlan(digest string) *generate.ModuleClientPlan {
	return &generate.ModuleClientPlan{
		Name: "saas-starter-accounts-client",
		Endpoint: composition.APIContractEndpoint{
			Service: "accounts", Endpoint: "connect", Kind: composition.APIContractKindProtobuf,
			Package: "saas.accounts.v1", Digest: digest,
		},
		Languages: []languages.Language{languages.GO, languages.TYPESCRIPT},
		Services:  []string{"AuditService"},
	}
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

func TestReconcileClientExportDecisions(t *testing.T) {
	manifest := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0"}
	plan := accountsPlan("sha256:aaaa")

	action, err := reconcileClientExport(manifest, plan, languages.GO)
	require.NoError(t, err)
	require.Equal(t, actionPublish, action, "nothing recorded: publish")

	manifest.record(plan, &PublishedClientExport{Language: "go", ImportPath: "github.com/codefly-dev/saas-starter-accounts-client-go"})

	action, err = reconcileClientExport(manifest, plan, languages.GO)
	require.NoError(t, err)
	require.Equal(t, actionSkip, action, "recorded at the same digest: skip")

	action, err = reconcileClientExport(manifest, plan, languages.TYPESCRIPT)
	require.NoError(t, err)
	require.Equal(t, actionPublish, action, "a sibling language not yet recorded still publishes")

	_, err = reconcileClientExport(manifest, accountsPlan("sha256:bbbb"), languages.TYPESCRIPT)
	require.ErrorIs(t, err, errContractMoved)
	require.Contains(t, err.Error(), "sha256:aaaa")
	require.Contains(t, err.Error(), "sha256:bbbb")
	require.Contains(t, err.Error(), composition.PackageManifestFileName)
}

func TestClientsManifestRoundTripIsCanonical(t *testing.T) {
	dir := t.TempDir()
	manifest := &ClientsManifest{Schema: ClientsManifestSchema, Package: "codefly/saas-starter", Version: "0.1.0"}
	gateway := &generate.ModuleClientPlan{Name: "saas-starter-auth-gateway-client",
		Endpoint: composition.APIContractEndpoint{Service: "auth-gateway", Endpoint: "grpc", Digest: "sha256:cccc"}}
	manifest.record(gateway, &PublishedClientExport{Language: "go", ImportPath: "g"})
	manifest.record(accountsPlan("sha256:aaaa"), &PublishedClientExport{Language: "typescript", ImportPath: "ts"})
	manifest.record(accountsPlan("sha256:aaaa"), &PublishedClientExport{Language: "go", ImportPath: "go-old"})
	manifest.record(accountsPlan("sha256:aaaa"), &PublishedClientExport{Language: "go", ImportPath: "go-new"})
	require.NoError(t, manifest.Save(t.Context(), dir))

	loaded, err := LoadClientsManifest(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"saas-starter-accounts-client", "saas-starter-auth-gateway-client"},
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
	manifest.record(plan, &PublishedClientExport{Language: "go", ImportPath: "g"})

	var out bytes.Buffer
	err := checkClients(&out, manifest, []generate.ModuleClientPlan{*plan})
	require.Error(t, err)
	require.Contains(t, err.Error(), "saas-starter-accounts-client typescript is not published at 0.1.0")
	require.NotContains(t, err.Error(), "saas-starter-accounts-client go is not")

	manifest.record(plan, &PublishedClientExport{Language: "typescript", ImportPath: "ts"})
	out.Reset()
	require.NoError(t, checkClients(&out, manifest, []generate.ModuleClientPlan{*plan}))
	require.Contains(t, out.String(), "codefly/saas-starter@0.1.0")

	err = checkClients(&out, manifest, []generate.ModuleClientPlan{*accountsPlan("sha256:bbbb")})
	require.Error(t, err)
	require.Contains(t, err.Error(), errContractMoved.Error())
	require.Contains(t, err.Error(), "bump the version")
}
