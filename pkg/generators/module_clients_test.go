package generators

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/stretchr/testify/require"
)

func writeModuleClientsConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ModuleClientsConfigFileName), []byte(yaml), 0o644))
	return dir
}

func TestLoadModuleClientsConfigIsOptionalAndStrict(t *testing.T) {
	config, err := LoadModuleClientsConfig(t.TempDir())
	require.NoError(t, err)
	require.Empty(t, config)

	dir := writeModuleClientsConfig(t, `schema: codefly/module-clients-config/v1
endpoints:
  - service: accounts
    endpoint: connect
    languages: [go, typescript]
    services: [AuditService, WebhookService]
  - service: marketing
    endpoint: http
    publish: false
`)
	config, err = LoadModuleClientsConfig(dir)
	require.NoError(t, err)
	require.Len(t, config, 2)
	require.Equal(t, []string{"go", "typescript"}, config["accounts/connect"].Languages)
	require.NotNil(t, config["marketing/http"].Publish)
	require.False(t, *config["marketing/http"].Publish)

	t.Run("unknown policy field", func(t *testing.T) {
		dir := writeModuleClientsConfig(t, `schema: codefly/module-clients-config/v1
endpoints:
  - service: accounts
    endpoint: connect
    publsih: false
`)
		_, err := LoadModuleClientsConfig(dir)
		require.ErrorContains(t, err, "publsih")
	})

	t.Run("duplicate endpoint", func(t *testing.T) {
		dir := writeModuleClientsConfig(t, `schema: codefly/module-clients-config/v1
endpoints:
  - {service: accounts, endpoint: connect}
  - {service: accounts, endpoint: connect}
`)
		_, err := LoadModuleClientsConfig(dir)
		require.ErrorContains(t, err, "configured twice")
	})
}

func testCatalog() *composition.APIContractCatalog {
	return &composition.APIContractCatalog{
		Schema: composition.APIContractCatalogSchema, Package: "codefly/saas-starter", Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{
			{Service: "accounts", Endpoint: "connect", API: "connect", Kind: composition.APIContractKindProtobuf,
				Package: "saas.accounts.v1", Path: "contracts/api/accounts/connect/contract.binpb", Digest: "sha256:aaaa",
				Services: []composition.APIContractService{{Name: "AuditService"}, {Name: "AuthService"}, {Name: "WebhookService"}}},
			{Service: "marketing", Endpoint: "rest", API: "rest", Kind: composition.APIContractKindOpenAPI,
				Package: "Marketing", Path: "contracts/api/marketing/rest/openapi.json", Digest: "sha256:bbbb"},
		},
	}
}

func TestPlanModuleClientsUsesEndpointQualifiedIdentities(t *testing.T) {
	catalog := testCatalog()
	catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{
		Service: "accounts", Endpoint: "grpc", Kind: composition.APIContractKindProtobuf, Package: "saas.accounts.v1", Digest: "sha256:cccc",
	})
	plans, err := PlanModuleClients("saas-starter", catalog, nil, nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"saas-starter-accounts-connect-client",
		"saas-starter-marketing-rest-client",
		"saas-starter-accounts-grpc-client",
	}, []string{plans[0].Name, plans[1].Name, plans[2].Name})
	for i := range plans {
		for j := range plans {
			if i != j {
				require.NotEqual(t, plans[i].Name, plans[j].Name)
			}
		}
	}
}

func TestPlanModuleClientsDefaultsToEveryLanguageTheKindSupports(t *testing.T) {
	plans, err := PlanModuleClients("saas-starter", testCatalog(), nil, nil)
	require.NoError(t, err)
	require.Len(t, plans, 2)
	require.Equal(t, []languages.Language{languages.GO, languages.TYPESCRIPT, languages.PYTHON}, plans[0].Languages)
	require.Nil(t, plans[0].Services)
	require.Equal(t, []languages.Language{languages.GO, languages.TYPESCRIPT}, plans[1].Languages)
}

func TestPlanModuleClientsHonoursEndpointPolicy(t *testing.T) {
	no := false
	config := map[string]EndpointClientsConfig{
		"accounts/connect": {Languages: []string{"typescript", " go "}, Services: []string{"WebhookService", "AuditService", "AuditService"}},
		"marketing/rest":   {Publish: &no},
	}
	plans, err := PlanModuleClients("saas-starter", testCatalog(), config, nil)
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Equal(t, []languages.Language{languages.TYPESCRIPT, languages.GO}, plans[0].Languages)
	require.Equal(t, []string{"AuditService", "WebhookService"}, plans[0].Services)
}

func TestPlanModuleClientsRestrictsToRequestedLanguages(t *testing.T) {
	plans, err := PlanModuleClients("saas-starter", testCatalog(), nil, []languages.Language{languages.PYTHON})
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Equal(t, "saas-starter-accounts-connect-client", plans[0].Name)
}

func TestPlanModuleClientsRejectsBadConfiguration(t *testing.T) {
	cases := map[string]struct {
		config map[string]EndpointClientsConfig
		want   string
	}{
		"unknown service in allowlist": {map[string]EndpointClientsConfig{"accounts/connect": {Services: []string{"AdminService"}}}, `"AdminService"`},
		"allowlist on openapi":         {map[string]EndpointClientsConfig{"marketing/rest": {Services: []string{"X"}}}, "only applies to protobuf"},
		"python for openapi":           {map[string]EndpointClientsConfig{"marketing/rest": {Languages: []string{"python"}}}, "no python client generator"},
		"unsupported language":         {map[string]EndpointClientsConfig{"accounts/connect": {Languages: []string{"cobol"}}}, `"cobol"`},
		"unknown endpoint":             {map[string]EndpointClientsConfig{"frontend/http": {Languages: []string{"go"}}}, "frontend/http"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := PlanModuleClients("saas-starter", testCatalog(), tc.config, nil)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestBuildModuleClientEntryUsesCommittedPackageArtifacts(t *testing.T) {
	dir := t.TempDir()
	contract := []byte("descriptor bytes")
	contractPath := filepath.Join(dir, "contracts", "api", "accounts", "connect", "contract.binpb")
	require.NoError(t, os.MkdirAll(filepath.Join(filepath.Dir(contractPath), "proto", "accounts", "v1"), 0o755))
	require.NoError(t, os.WriteFile(contractPath, contract, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(contractPath), "proto", "accounts", "v1", "accounts.proto"), []byte("syntax = \"proto3\";"), 0o644))
	catalog := &composition.APIContractCatalog{Package: "codefly/saas-starter", Version: "0.1.0"}
	plan := &ModuleClientPlan{
		Endpoint: composition.APIContractEndpoint{
			Service: "accounts", Endpoint: "connect", Kind: composition.APIContractKindProtobuf,
			Path: "contracts/api/accounts/connect/contract.binpb", Digest: composition.APIContractDigest(contract),
		},
		Services: []string{"AccountsService"},
	}
	entry, err := BuildModuleClientEntry(t.Context(), dir, "saas-starter", catalog, plan)
	require.NoError(t, err)
	require.Equal(t, contract, entry.ContractBytes)
	require.Equal(t, "accounts/v1/accounts.proto", entry.ProtoSources[0].Path)
	require.Equal(t, []string{"AccountsService"}, entry.Services)
}
