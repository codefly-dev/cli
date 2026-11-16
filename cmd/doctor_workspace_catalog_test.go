package cmd

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// catalogWorkspace publishes, for module backend, a sandbox service whose
// descriptor serves Run, RunMounted and Version on two listeners, each
// listing the given methods.
func catalogWorkspace(t *testing.T, listed ...string) string {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name: googleproto.String("fixture/sandbox/v1/api.proto"), Package: googleproto.String("fixture.sandbox.v1"),
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: googleproto.String("SandboxService")}},
	}
	for _, method := range []string{"Run", "RunMounted", "Version"} {
		file.Service[0].Method = append(file.Service[0].Method, &descriptorpb.MethodDescriptorProto{Name: googleproto.String(method)})
	}
	descriptor, err := googleproto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{file}})
	if err != nil {
		t.Fatal(err)
	}
	service := composition.APIContractService{Name: "SandboxService", FullName: "fixture.sandbox.v1.SandboxService"}
	for _, method := range listed {
		service.Procedures = append(service.Procedures, "/fixture.sandbox.v1.SandboxService/"+method)
	}
	catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "acme/backend", Version: "0.1.0"}
	files := map[string]string{}
	for _, endpoint := range []string{"connect", "grpc"} {
		path := "contracts/api/sandbox/" + endpoint + "/contract.binpb"
		files["modules/backend/"+path] = string(descriptor)
		catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{
			Service: "sandbox", Endpoint: endpoint, API: endpoint, Kind: composition.APIContractKindProtobuf,
			Package: "fixture.sandbox.v1", Path: path, Digest: composition.APIContractDigest(descriptor),
			Services: []composition.APIContractService{service},
		})
	}
	data, err := catalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	files["modules/backend/"+composition.APIContractCatalogFileName] = string(data)
	return singleServiceWorkspace(t, testWorkspaceYAML, nil, files)
}

func TestDoctorWorkspaceRefusesACatalogThatOmitsAServedMethod(t *testing.T) {
	report := runReadiness(t, workspaceReadinessOptions{dir: catalogWorkspace(t, "Run", "Version")})
	got := requireCode(t, report, codeContractCatalogUnserved, "warn")
	if !strings.Contains(got.Message, "/fixture.sandbox.v1.SandboxService/RunMounted") {
		t.Fatalf("the finding must name the unserved method: %+v", got)
	}
}

func TestDoctorWorkspaceAcceptsACatalogThatListsEveryServedMethod(t *testing.T) {
	report := runReadiness(t, workspaceReadinessOptions{dir: catalogWorkspace(t, "Run", "RunMounted", "Version")})
	requireNoCode(t, report, codeContractCatalogUnserved)
}
