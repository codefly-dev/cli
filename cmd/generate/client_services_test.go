package generate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

// writeTwoServicesContractsFixture compiles a contract of two services over a
// shared file into a module's committed contracts/api directory: WantedService
// (one file, setting a google.api.http option) and OtherService (another file,
// the only one importing empty.proto), both naming declarations of
// acme/v1/common.proto, where OtherOnly and Spare serve neither.
func writeTwoServicesContractsFixture(t *testing.T) string {
	t.Helper()
	moduleDir := t.TempDir()
	protoDir := filepath.Join(moduleDir, "proto")
	writeTestFile(t, filepath.Join(protoDir, "buf.yaml"), "version: v1\ndeps:\n  - buf.build/googleapis/googleapis\n")
	writeTestFile(t, filepath.Join(protoDir, "acme", "v1", "common.proto"), `syntax = "proto3";

package acme.v1;

import "google/protobuf/timestamp.proto";

// Shared is what both services name.
message Shared {
  string id = 1;
  google.protobuf.Timestamp at = 2;
}

message OtherOnly {
  string note = 1;
}

message Spare {
  string unused = 1;
}

enum Color {
  COLOR_UNSPECIFIED = 0;
  COLOR_RED = 1;
}
`)
	writeTestFile(t, filepath.Join(protoDir, "acme", "v1", "wanted.proto"), `syntax = "proto3";

package acme.v1;

import "acme/v1/common.proto";
import "google/api/annotations.proto";

message GetWantedRequest {
  Shared shared = 1;
  Color color = 2;
}

message GetWantedResponse {
  Shared shared = 1;
}

// WantedService is the one service the library binds.
service WantedService {
  rpc GetWanted(GetWantedRequest) returns (GetWantedResponse) {
    option (google.api.http) = {get: "/v1/wanted"};
  }
}
`)
	writeTestFile(t, filepath.Join(protoDir, "acme", "v1", "other.proto"), `syntax = "proto3";

package acme.v1;

import "acme/v1/common.proto";
import "google/protobuf/empty.proto";

message DoOtherRequest {
  OtherOnly other = 1;
  Shared shared = 2;
}

service OtherService {
  rpc DoOther(DoOtherRequest) returns (google.protobuf.Empty);
}
`)
	descriptorSet, err := buildDescriptorSet(context.Background(), protoDir)
	if err != nil {
		t.Fatalf("buildDescriptorSet: %v", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(descriptorSet, &set); err != nil {
		t.Fatal(err)
	}
	return writeContractsCatalog(t, moduleDir, "codefly/acme", descriptorSet, composition.APIContractEndpoint{
		Service:  "api",
		Endpoint: "connect",
		API:      "connect",
		Kind:     composition.APIContractKindProtobuf,
		Package:  "acme.v1",
		Services: composition.ProtobufServices(&set, "acme.v1"),
	})
}

// generateWantedClient runs `generate client --services WantedService` over
// the two-service contract into output, in every client language.
func generateWantedClient(t *testing.T, contractsDir, output string) {
	t.Helper()
	resetClientFlags(t)
	clientFrom = "contracts:" + contractsDir
	clientLanguages = []string{"go", "typescript", "python"}
	clientServices = []string{"WantedService"}
	clientName = "wanted-client"
	clientGoModule = "github.com/codefly-dev/wanted-client-go"
	clientOutput = output
	if err := ClientCmd.RunE(ClientCmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
}

func qualifyWorkspace(t *testing.T) {
	t.Helper()
	workspace := &resources.Workspace{Name: "qualify-ws", Layout: resources.LayoutKindModules}
	wsDir := filepath.Join(t.TempDir(), "ws")
	if err := workspace.SaveToDirUnsafe(context.Background(), wsDir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(wsDir)
}

// TestGenerateClientServicesNarrowsTheWholeLibrary: a library generated for
// one service of a contract carries that service alone (#1173). Its persisted
// descriptor set declares that service and the declarations it reaches, its
// catalog and manifest list only it (the manifest at the producer's digest,
// which identifies the source), and no language's bindings or facade
// mention the other service or what only the other service needs — and each
// still builds.
func TestGenerateClientServicesNarrowsTheWholeLibrary(t *testing.T) {
	requireQualify(t)
	contractsDir := writeTwoServicesContractsFixture(t)
	qualifyWorkspace(t)
	output := filepath.Join(t.TempDir(), "lib")
	generateWantedClient(t, contractsDir, output)

	contract, err := os.ReadFile(filepath.Join(output, "contract", "contract.binpb"))
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(contract, &set); err != nil {
		t.Fatal(err)
	}
	var files, services, ownDeclarations []string
	for _, file := range set.GetFile() {
		files = append(files, file.GetName())
		for _, service := range file.GetService() {
			services = append(services, file.GetPackage()+"."+service.GetName())
		}
		if file.GetPackage() != "acme.v1" {
			continue
		}
		for _, message := range file.GetMessageType() {
			ownDeclarations = append(ownDeclarations, message.GetName())
		}
		for _, enum := range file.GetEnumType() {
			ownDeclarations = append(ownDeclarations, enum.GetName())
		}
	}
	if !slices.Equal(services, []string{"acme.v1.WantedService"}) {
		t.Errorf("contract services = %v, want only acme.v1.WantedService", services)
	}
	sort.Strings(ownDeclarations)
	if want := []string{"Color", "GetWantedRequest", "GetWantedResponse", "Shared"}; !slices.Equal(ownDeclarations, want) {
		t.Errorf("contract declares %v, want exactly what WantedService reaches: %v", ownDeclarations, want)
	}
	for _, dropped := range []string{"acme/v1/other.proto", "google/protobuf/empty.proto"} {
		if slices.Contains(files, dropped) {
			t.Errorf("contract still carries %s: %v", dropped, files)
		}
	}
	if !slices.Contains(files, "google/api/annotations.proto") {
		t.Errorf("contract lost the file defining the option WantedService sets: %v", files)
	}

	var catalog composition.APIContractCatalog
	catalogData, err := os.ReadFile(filepath.Join(output, "contract", "catalog.codefly.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(catalogData, &catalog); err != nil {
		t.Fatal(err)
	}
	endpoint := catalog.Endpoints[0]
	if len(endpoint.Services) != 1 || endpoint.Services[0].FullName != "acme.v1.WantedService" {
		t.Errorf("catalog services = %+v, want only acme.v1.WantedService", endpoint.Services)
	}
	if endpoint.Digest != composition.APIContractDigest(contract) {
		t.Errorf("catalog digest %s is not the persisted contract's %s", endpoint.Digest, composition.APIContractDigest(contract))
	}

	var manifest struct {
		Sources []struct {
			ContractDigest string   `yaml:"contract-digest"`
			Services       []string `yaml:"services"`
		} `yaml:"sources"`
	}
	manifestData, err := os.ReadFile(filepath.Join(output, resources.LibraryConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	producer, err := composition.LoadAPIContractCatalog(filepath.Dir(filepath.Dir(contractsDir)))
	if err != nil {
		t.Fatal(err)
	}
	if source := manifest.Sources[0]; !slices.Equal(source.Services, []string{"WantedService"}) || source.ContractDigest != producer.Endpoints[0].Digest {
		t.Errorf("manifest source = %+v, want services [WantedService] at the producer's digest %s", source, producer.Endpoints[0].Digest)
	}

	for _, lang := range []string{"go", "typescript", "python"} {
		assertTreeLacks(t, filepath.Join(output, lang), "OtherService", "DoOther", "OtherOnly", "Spare")
	}
	assertFileContains(t, findFacadeFile(t, filepath.Join(output, "go"), "_facade.pb.go"), "WantedService")
	assertFileContains(t, findFacadeFile(t, filepath.Join(output, "typescript"), "_facade.ts"), "WantedService")
	assertFileContains(t, findFacadeFile(t, filepath.Join(output, "python", "wanted_client"), "acme.py"), "class WantedServiceClient")
	goVetLibrary(t, filepath.Join(output, "go"))
	assertPythonImports(t, filepath.Join(output, "python"), "wanted_client.acme")
}

// TestGenerateClientServicesIsStable is the drift check a committed library is
// gated with: regenerating in place, and generating afresh into another tree,
// both reproduce the library byte for byte.
func TestGenerateClientServicesIsStable(t *testing.T) {
	requireQualify(t)
	contractsDir := writeTwoServicesContractsFixture(t)
	qualifyWorkspace(t)
	output := filepath.Join(t.TempDir(), "lib")
	generateWantedClient(t, contractsDir, output)
	first := hashTree(t, output)

	generateWantedClient(t, contractsDir, output)
	if again := hashTree(t, output); again != first {
		t.Fatalf("regenerating in place drifted: %s != %s", again, first)
	}

	fresh := filepath.Join(t.TempDir(), "lib")
	generateWantedClient(t, contractsDir, fresh)
	if afresh := hashTree(t, fresh); afresh != first {
		t.Fatalf("generating afresh drifted from the library: %s != %s", afresh, first)
	}
}

// assertTreeLacks fails for every file under root mentioning any of words.
func assertTreeLacks(t *testing.T, root string, words ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, word := range words {
			if strings.Contains(string(data), word) {
				t.Errorf("%s mentions %s", path, word)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestGenerateClientServicesSelectionChangesRegenerateWithoutForce: what a
// library records as its source is the producer's contract, so editing an
// entry's services and adding an entry regenerate in place without --force
// (sync solution-sdk does both on every api.consumes edit). Each entry's
// persisted contract declares its own selection alone, both record the
// producer digest `publish clients` records, and regenerating is stable.
// Python, because one Go package cannot hold two facades of different names.
func TestGenerateClientServicesSelectionChangesRegenerateWithoutForce(t *testing.T) {
	requireQualify(t)
	contractsDir := writeTwoServicesContractsFixture(t)
	catalog, err := composition.LoadAPIContractCatalog(filepath.Dir(filepath.Dir(contractsDir)))
	if err != nil {
		t.Fatal(err)
	}
	producer := catalog.Endpoints[0]
	contract, err := generators.ReadCatalogContractBytes(filepath.Dir(filepath.Dir(contractsDir)), &producer)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(endpoint, as string, services ...string) generators.ContractEntry {
		e := producer
		e.Endpoint = endpoint
		return generators.ContractEntry{
			Endpoint: e, ContractBytes: contract, PackageID: catalog.Package, PackageVersion: catalog.Version,
			ModuleName: "acme", FacadeModuleDefault: generators.DefaultFacadeModuleName(&e), Services: services, As: as,
		}
	}
	output := filepath.Join(t.TempDir(), "lib")
	generate := func(entries ...generators.ContractEntry) {
		t.Helper()
		if err := generators.GenerateClientLibrary(context.Background(), &generators.ClientLibraryRequest{
			Name: "acme-sdk", Output: output, Languages: []languages.Language{languages.PYTHON}, Entries: entries,
		}); err != nil {
			t.Fatalf("regenerate without --force: %v", err)
		}
	}

	generate(entry("connect", "", "WantedService"))
	generate(entry("connect", "", "WantedService", "OtherService"))
	both := []generators.ContractEntry{entry("connect", "", "WantedService"), entry("connect-other", "other", "OtherService")}
	generate(both...)
	first := hashTree(t, output)
	generate(both...)
	if again := hashTree(t, output); again != first {
		t.Fatalf("regenerating the aggregated library drifted: %s != %s", again, first)
	}

	for dir, want := range map[string]string{"acme-api-connect": "acme.v1.WantedService", "acme-api-connect-other": "acme.v1.OtherService"} {
		data, err := os.ReadFile(filepath.Join(output, "contract", dir, "contract.binpb"))
		if err != nil {
			t.Fatal(err)
		}
		var set descriptorpb.FileDescriptorSet
		if err := googleproto.Unmarshal(data, &set); err != nil {
			t.Fatal(err)
		}
		var services []string
		for _, file := range set.GetFile() {
			for _, service := range file.GetService() {
				services = append(services, file.GetPackage()+"."+service.GetName())
			}
		}
		if !slices.Equal(services, []string{want}) {
			t.Errorf("%s persists services %v, want only %s", dir, services, want)
		}
	}
	var manifest struct {
		Sources []struct {
			ContractDigest string `yaml:"contract-digest"`
		} `yaml:"sources"`
	}
	manifestData, err := os.ReadFile(filepath.Join(output, resources.LibraryConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, source := range manifest.Sources {
		if source.ContractDigest != producer.Digest {
			t.Errorf("manifest records contract-digest %s, want the producer's %s", source.ContractDigest, producer.Digest)
		}
	}
	assertPythonImports(t, filepath.Join(output, "python"), "acme_sdk.acme")
	assertPythonImports(t, filepath.Join(output, "python"), "acme_sdk.other")
}
