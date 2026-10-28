package generate

import (
	"context"
	"fmt"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestContractSurfaceExportAndDerivation(t *testing.T) {
	ctx := context.Background()
	service := &resources.Service{Name: "runtime-worker", Version: "0.1.0",
		Agent:     &resources.Agent{Kind: resources.ServiceAgent, Name: "go", Publisher: "codefly.dev", Version: "0.0.48"},
		Endpoints: []*resources.Endpoint{{Name: "invoke", API: "connect"}, {Name: "receipts", API: "grpc"}},
	}
	root, moduleDir := saveFixtureWorkspace(t, ctx, "documents", nil, service)
	saveRunnablePublication(t, moduleDir, "documents.ingestion",
		modulecontract.Destination{ID: "invoke", Service: service.Name, Endpoint: "invoke", Kind: "module"},
		modulecontract.Destination{ID: "receipts", Service: service.Name, Endpoint: "receipts", Kind: "module"})
	service, err := resources.LoadServiceFromDir(ctx, service.Dir())
	require.NoError(t, err)
	writeFixtureFile(t, filepath.Join(service.Dir(), "generated/surfaces.json"), []byte(`{
  "invoke": [{"name":"IngestionService","fullName":"documents.ingest.v1.IngestionService","procedures":["/documents.ingest.v1.IngestionService/ApplyText","/documents.ingest.v1.IngestionService/LookupText"]}],
  "receipts": [{"name":"IngestionService","fullName":"documents.ingest.v1.IngestionService","procedures":["/documents.ingest.v1.IngestionService/LookupText"]}]
}`))
	endpoints := []*basev0.Endpoint{{Name: "invoke", Api: "connect"}, {Name: "receipts", Api: "grpc"}}
	surfacePath := fixtureSurfacePath(t, moduleDir, service.Name, "generated/surfaces.json")
	surfaces, err := loadContractSurfaces(service, endpoints, surfacePath)
	require.NoError(t, err)
	module := &resources.Module{Name: "documents"}
	module.WithDir(moduleDir)
	file := ingestionFile(conformingOperation())
	contract := descriptorSet(t, file)
	writeFixtureFile(t, filepath.Join(service.Dir(), "proto", file.GetName()), []byte("// descriptor input is supplied by the test\n"))
	catalog := &composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "codefly/documents", Version: "0.1.0"}
	for _, endpoint := range endpoints {
		dir := filepath.Join(moduleDir, "contracts/api", service.Name, endpoint.Name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		entry, writeErr := writeBuiltProtobufContract(ctx, module, service, endpoint, surfaces[endpoint.Name], dir, dir, contract)
		require.NoError(t, writeErr)
		catalog.Endpoints = append(catalog.Endpoints, *entry)
		written, readErr := os.ReadFile(filepath.Join(dir, "contract.binpb"))
		require.NoError(t, readErr)
		require.Equal(t, contract, written, "descriptor bytes remain complete on both endpoints")
	}
	require.NoError(t, catalog.Validate())
	data, err := catalog.CanonicalBytes()
	require.NoError(t, err)
	writeFixtureFile(t, filepath.Join(moduleDir, composition.APIContractCatalogFileName), data)
	t.Chdir(root)
	resetRunnablesFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	index := readIndex(t, moduleDir)
	require.Len(t, index.Operations, 1)
	require.Equal(t, "invoke", index.Operations[0].Endpoint)
}

func TestGenerateContractsRequiresSurfaceBeforeLoadingBuilder(t *testing.T) {
	service := &resources.Service{Name: "owner", Version: "0.1.0",
		Agent:     &resources.Agent{Kind: resources.ServiceAgent, Name: "go", Publisher: "codefly.dev", Version: "0.0.48"},
		Endpoints: []*resources.Endpoint{{Name: "grpc", API: "grpc"}, {Name: "connect", API: "connect"}},
	}
	root, _ := saveFixtureWorkspace(t, context.Background(), "documents",
		&resources.ModuleInterface{Endpoints: []*resources.InterfaceEndpoint{
			{Service: "owner", Endpoint: "grpc", Visibility: resources.VisibilityInternal},
			{Service: "owner", Endpoint: "connect", Visibility: resources.VisibilityInternal},
		}}, service)
	writeFixtureFile(t, filepath.Join(service.Dir(), "proto/buf.yaml"), []byte("version: v2\n"))
	t.Chdir(root)
	resetContractsFlags(t)
	err := ContractsCmd.RunE(ContractsCmd, []string{"documents"})
	require.ErrorContains(t, err, "contracts.codefly.yaml")
	require.ErrorContains(t, err, "module documents")
	require.ErrorContains(t, err, "surfaces: {owner:")
	require.ErrorContains(t, err, `"procedures":`)
	require.ErrorContains(t, err, "docs/commands.md#generate-contracts")
	require.NotContains(t, err.Error(), "generated")
	require.ErrorContains(t, err, "exports multiple protobuf endpoints")
}

func TestContractSurfacesRequireCompleteUnambiguousInventory(t *testing.T) {
	service := &resources.Service{Name: "owner", Endpoints: []*resources.Endpoint{{Name: "grpc"}, {Name: "connect"}}}
	service.WithDir(t.TempDir())
	endpoints := []*basev0.Endpoint{{Name: "grpc", Api: "grpc"}, {Name: "connect", Api: "connect"}}
	_, err := loadContractSurfaces(service, endpoints, "")
	require.ErrorContains(t, err, "exports multiple protobuf endpoints")
	_, err = loadContractSurfaces(service, endpoints[:1], "")
	require.NoError(t, err, "a single full-surface export retains existing behavior")
	surfacePath := "surfaces.json"
	for _, tc := range []struct{ name, data, want string }{
		{"missing endpoint", `{"grpc":[{"name":"Svc","fullName":"test.Svc","procedures":[]}]}`, "no service inventory for endpoint connect"},
		{"unknown endpoint", `{"conect":[]}`, "undeclared endpoint conect"},
		{"unknown field", `{"grpc":[{"methods":[]}]}`, `unknown field "methods"`},
		{"trailing document", `{} {}`, "expected one JSON document"},
		{"null", `null`, "no service inventory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFixtureFile(t, filepath.Join(service.Dir(), "surfaces.json"), []byte(tc.data))
			_, err := loadContractSurfaces(service, endpoints, surfacePath)
			require.ErrorContains(t, err, tc.want)
		})
	}
	surfacePath = "../surfaces.json"
	_, err = loadContractSurfaces(service, endpoints, surfacePath)
	require.Error(t, err, "inventory must stay within its owner service")
}

func fixtureSurfacePath(t *testing.T, moduleDir, service, path string) string {
	t.Helper()
	writeFixtureFile(t, filepath.Join(moduleDir, contractSurfacesConfigFile), []byte("schema: "+contractSurfacesConfigSchema+"\nsurfaces:\n  "+service+": "+path+"\n"))
	module := &resources.Module{Name: "fixture", ServiceReferences: []*resources.ServiceReference{{Name: service}}}
	module.WithDir(moduleDir)
	paths, err := loadContractSurfacePaths(module)
	require.NoError(t, err)
	return paths[service]
}

func TestContractSurfaceConfigurationIsCLIOwnedAndStrict(t *testing.T) {
	module := &resources.Module{Name: "fixture", ServiceReferences: []*resources.ServiceReference{{Name: "owner"}}}
	module.WithDir(t.TempDir())
	for _, tc := range []struct{ data, want string }{
		{"schema: wrong\n", "schema must be"},
		{"schema: " + contractSurfacesConfigSchema + "\nsurfaces: {typo: surfaces.json}\n", "declared service"},
		{"schema: " + contractSurfacesConfigSchema + "\nsurfaces: {owner: ''}\n", "service-relative JSON"},
		{"schema: " + contractSurfacesConfigSchema + "\nunknown: true\n", "field unknown"},
		{"schema: " + contractSurfacesConfigSchema + "\n---\n{}", "one YAML document"},
	} {
		writeFixtureFile(t, filepath.Join(module.Dir(), contractSurfacesConfigFile), []byte(tc.data))
		_, err := loadContractSurfacePaths(module)
		require.ErrorContains(t, err, tc.want)
	}
	service := &resources.Service{Name: "owner", Spec: map[string]any{"api-contract-surfaces": "ignored-agent-setting.json"}}
	_, err := loadContractSurfaces(service, []*basev0.Endpoint{{Name: "grpc", Api: "grpc"}, {Name: "connect", Api: "connect"}}, "")
	require.ErrorContains(t, err, contractSurfacesConfigFile, "agent settings are not CLI configuration")
}

// An implementing listener serves its own API and several host interfaces. Its
// generated inventory, rather than a proto package filter, defines publication.
func TestContractSurfacePublishesImplementedInterfacePackages(t *testing.T) {
	ctx := context.Background()
	service := &resources.Service{Name: "accounts"}
	module := &resources.Module{Name: "saas"}
	module.WithDir(t.TempDir())
	service.WithDir(filepath.Join(module.Dir(), "services/accounts"))
	var files []*descriptorpb.FileDescriptorProto
	var surface []composition.APIContractService
	for _, entry := range []struct {
		pkg, name string
		count     int
	}{
		{"saas.accounts.v1", "Accounts", 1},
		{"host.authority.v1", "AuthorityService", 7},
		{"host.installation.v1", "InstallationService", 3},
		{"host.session.v1", "SessionService", 3},
		{"host.composition.v1", "CompositionService", 1},
		{"foreign.client.v1", "NotServed", 1},
	} {
		f := &descriptorpb.FileDescriptorProto{Name: googleproto.String(strings.ReplaceAll(entry.pkg, ".", "/") + "/api.proto"), Package: googleproto.String(entry.pkg),
			Service: []*descriptorpb.ServiceDescriptorProto{{Name: googleproto.String(entry.name)}},
		}
		for i := 0; i < entry.count; i++ {
			f.Service[0].Method = append(f.Service[0].Method, &descriptorpb.MethodDescriptorProto{Name: googleproto.String(fmt.Sprintf("Call%d", i))})
		}
		files = append(files, f)
		// Implemented schemas can be imports rather than physically owned files.
		if strings.HasPrefix(entry.pkg, "saas.") || strings.HasPrefix(entry.pkg, "foreign.") {
			writeFixtureFile(t, filepath.Join(service.Dir(), "proto", f.GetName()), []byte("// source snapshot\n"))
		}
		if entry.name != "NotServed" {
			surface = append(surface, composition.ProtobufServices(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{f}}, entry.pkg)...)
		}
	}
	set := &descriptorpb.FileDescriptorSet{File: files}
	data, err := googleproto.Marshal(set)
	require.NoError(t, err)
	dir := filepath.Join(module.Dir(), "contracts/api/accounts/connect")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	endpoint := &basev0.Endpoint{Name: "connect", Api: "connect"}
	entry, err := writeBuiltProtobufContract(ctx, module, service, endpoint, surface, dir, dir, data)
	require.NoError(t, err)
	require.Empty(t, entry.Package, "there is no single package; fullName carries each service's package")
	require.Equal(t, surface, entry.Services)
	methods, err := endpointProcedures(set, entry)
	require.NoError(t, err)
	require.Len(t, methods, 15, "own method plus all 14 implemented host procedures")
	for _, method := range methods {
		require.NotContains(t, method, "NotServed")
	}
	catalog := &composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "codefly/saas", Version: "0.1.0", Endpoints: []composition.APIContractEndpoint{*entry}}
	require.NoError(t, catalog.Validate())
	encoded, err := catalog.CanonicalBytes()
	require.NoError(t, err)
	writeFixtureFile(t, filepath.Join(module.Dir(), composition.APIContractCatalogFileName), encoded)
	loaded, err := composition.LoadAPIContractCatalog(module.Dir())
	require.NoError(t, err)
	require.Len(t, loaded.Endpoints[0].Services, 5)
	// A typo or a method missing from the descriptor must still be a named refusal.
	entry.Services[1].Procedures = append(entry.Services[1].Procedures, "/host.authority.v1.AuthorityService/Missing")
	_, err = endpointProcedures(set, entry)
	require.ErrorContains(t, err, "/host.authority.v1.AuthorityService/Missing")
}
