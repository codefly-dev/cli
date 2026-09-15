package generate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func writeModuleManifest(t *testing.T, yaml string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, resources.ModuleConfigurationName), []byte(yaml), 0o644))
	return dir
}

func TestLoadModuleClientsConfigReadsOnlyDeclaredBlocks(t *testing.T) {
	dir := writeModuleManifest(t, `name: saas-starter
interface:
  endpoints:
    - service: accounts
      endpoint: connect
      visibility: module
      clients:
        languages: [go, typescript]
        services: [AuditService, WebhookService]
    - service: auth-gateway
      endpoint: grpc
      visibility: module
    - service: marketing
      endpoint: http
      visibility: public
      clients:
        publish: false
`)
	config, err := LoadModuleClientsConfig(dir)
	require.NoError(t, err)
	require.Len(t, config, 2)
	accounts := config["accounts/connect"]
	require.Nil(t, accounts.Publish)
	require.Equal(t, []string{"go", "typescript"}, accounts.Languages)
	require.Equal(t, []string{"AuditService", "WebhookService"}, accounts.Services)
	marketing := config["marketing/http"]
	require.NotNil(t, marketing.Publish)
	require.False(t, *marketing.Publish)
	_, declared := config["auth-gateway/grpc"]
	require.False(t, declared, "an endpoint without a clients: block must not appear")
}

// The block is side-read: core's own module decoder must keep accepting a
// manifest that carries it, or every module declaring clients would stop
// loading everywhere else.
func TestModuleManifestWithClientsBlockStillDecodesInCore(t *testing.T) {
	module, err := resources.LoadFromBytes[resources.Module]([]byte(`name: saas-starter
interface:
  endpoints:
    - service: accounts
      endpoint: connect
      clients:
        languages: [go]
`))
	require.NoError(t, err)
	require.Equal(t, "saas-starter", module.Name)
	require.Len(t, module.Interface.Endpoints, 1)
	require.Equal(t, "accounts", module.Interface.Endpoints[0].Service)
}

func testCatalog() *composition.APIContractCatalog {
	return &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/saas-starter",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{
			{
				Service: "accounts", Endpoint: "connect", API: "connect", Kind: composition.APIContractKindProtobuf,
				Package: "saas.accounts.v1", Path: "accounts/connect/contract.binpb", Digest: "sha256:aaaa",
				Services: []composition.APIContractService{
					{Name: "AuditService", FullName: "saas.accounts.v1.AuditService"},
					{Name: "AuthService", FullName: "saas.accounts.v1.AuthService"},
					{Name: "WebhookService", FullName: "saas.accounts.v1.WebhookService"},
				},
			},
			{
				Service: "marketing", Endpoint: "rest", API: "rest", Kind: composition.APIContractKindOpenAPI,
				Package: "Marketing", Path: "marketing/rest/openapi.json", Digest: "sha256:bbbb",
			},
		},
	}
}

func TestPlanModuleClientsDefaultsToEveryLanguageTheKindSupports(t *testing.T) {
	module := &resources.Module{Name: "saas-starter"}
	plans, err := PlanModuleClients(module, testCatalog(), nil, nil)
	require.NoError(t, err)
	require.Len(t, plans, 2)

	require.Equal(t, "saas-starter-accounts-client", plans[0].Name)
	require.Equal(t, []languages.Language{languages.GO, languages.TYPESCRIPT, languages.PYTHON}, plans[0].Languages)
	require.Nil(t, plans[0].Services, "no allowlist means the whole contract")

	require.Equal(t, "saas-starter-marketing-client", plans[1].Name)
	require.Equal(t, []languages.Language{languages.GO, languages.TYPESCRIPT}, plans[1].Languages, "python has no OpenAPI client generator")
}

func TestPlanModuleClientsHonoursTheEndpointBlock(t *testing.T) {
	module := &resources.Module{Name: "saas-starter"}
	no := false
	config := map[string]EndpointClientsConfig{
		"accounts/connect": {Languages: []string{"typescript", " go "}, Services: []string{"WebhookService", "AuditService"}},
		"marketing/rest":   {Publish: &no},
	}
	plans, err := PlanModuleClients(module, testCatalog(), config, nil)
	require.NoError(t, err)
	require.Len(t, plans, 1, "publish: false opts the endpoint out")
	require.Equal(t, []languages.Language{languages.TYPESCRIPT, languages.GO}, plans[0].Languages)
	require.Equal(t, []string{"AuditService", "WebhookService"}, plans[0].Services, "allowlist is sorted for stable manifests")
}

func TestPlanModuleClientsRestrictsToRequestedLanguages(t *testing.T) {
	module := &resources.Module{Name: "saas-starter"}
	plans, err := PlanModuleClients(module, testCatalog(), nil, []languages.Language{languages.PYTHON})
	require.NoError(t, err)
	require.Len(t, plans, 1, "the OpenAPI endpoint has no python client and drops out")
	require.Equal(t, "saas-starter-accounts-client", plans[0].Name)
	require.Equal(t, []languages.Language{languages.PYTHON}, plans[0].Languages)
}

func TestPlanModuleClientsRejectsBadConfiguration(t *testing.T) {
	module := &resources.Module{Name: "saas-starter"}
	cases := map[string]struct {
		config map[string]EndpointClientsConfig
		want   string
	}{
		"unknown service in allowlist": {
			config: map[string]EndpointClientsConfig{"accounts/connect": {Services: []string{"AdminService"}}},
			want:   `"AdminService"`,
		},
		"allowlist on an openapi contract": {
			config: map[string]EndpointClientsConfig{"marketing/rest": {Services: []string{"X"}}},
			want:   "only applies to protobuf",
		},
		"python client for openapi": {
			config: map[string]EndpointClientsConfig{"marketing/rest": {Languages: []string{"python"}}},
			want:   "no python client generator",
		},
		"unsupported language": {
			config: map[string]EndpointClientsConfig{"accounts/connect": {Languages: []string{"cobol"}}},
			want:   `"cobol"`,
		},
		"clients block on an endpoint that exports no contract": {
			config: map[string]EndpointClientsConfig{"frontend/http": {Languages: []string{"go"}}},
			want:   "frontend/http",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := PlanModuleClients(module, testCatalog(), tc.config, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}
