package generate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/encoding/protojson"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

const primaryDocumentsPackage = "acme.documents.v1"

// documentsFile is the owner's primary API, in its own package, declaring an
// unmarked method: the marked operation lives in the second package, the
// ingestion service, which is what a multi-package endpoint has to reach.
func documentsFile(methods ...*descriptorpb.MethodDescriptorProto) *descriptorpb.FileDescriptorProto {
	if len(methods) == 0 {
		methods = []*descriptorpb.MethodDescriptorProto{
			markedMethod("GetDocument", ".documents.ingest.v1.LookupTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil),
		}
	}
	return &descriptorpb.FileDescriptorProto{
		Name:       googleproto.String("acme/documents/v1/api.proto"),
		Package:    googleproto.String(primaryDocumentsPackage),
		Syntax:     googleproto.String("proto3"),
		Dependency: []string{"codefly/runnable/v0/options.proto", "documents/ingest/v1/ingestion.proto"},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name:   googleproto.String("DocumentService"),
			Method: methods,
		}},
	}
}

// multiPackageContract is the descriptor set `generate contracts` writes for
// a service whose own protos declare services in two packages.
func multiPackageContract(t *testing.T, primary *descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	set := mustParseSet(t, descriptorSet(t, ingestionFile(conformingOperation())))
	set.File = append(set.File, primary)
	data, err := googleproto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// publishMultiPackageContract writes the catalog entry `generate contracts`
// records for that service: Package is the primary package, Services spans
// both packages.
func publishMultiPackageContract(t *testing.T, moduleDir string, contract []byte) {
	t.Helper()
	const contractPath = "contracts/api/runtime-worker/grpc/contract.binpb"
	writeFixtureFile(t, filepath.Join(moduleDir, filepath.FromSlash(contractPath)), contract)
	set := mustParseSet(t, contract)
	services := append(composition.ProtobufServices(set, primaryDocumentsPackage), composition.ProtobufServices(set, ingestPackage)...)
	catalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/documents",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "runtime-worker",
			Endpoint: "grpc",
			API:      "grpc",
			Kind:     composition.APIContractKindProtobuf,
			Package:  primaryDocumentsPackage,
			Path:     contractPath,
			Digest:   composition.APIContractDigest(contract),
			Services: services,
		}},
	}
	catalogBytes, err := catalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(moduleDir, filepath.FromSlash(composition.APIContractCatalogFileName)), catalogBytes)
}

// A method marked in the endpoint's second package is derived and bound just
// like one in its primary package.
func TestGenerateRunnablesDerivesAndBindsAMethodInASecondPackage(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	publishMultiPackageContract(t, moduleDir, multiPackageContract(t, documentsFile()))

	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	index := readIndex(t, moduleDir)
	if len(index.Operations) != 1 || index.Operations[0].Method != applyTextMethod {
		t.Fatalf("operations = %+v, want the second package's %s", index.Operations, applyTextMethod)
	}

	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a header and one binding:\n%s", raw)
	}
	_, value, _ := strings.Cut(lines[1], "=")
	var prepared PreparedRunnable
	if err = json.Unmarshal([]byte(value), &prepared); err != nil {
		t.Fatal(err)
	}
	pkg := &basev0.RunnablePackage{}
	binding := &basev0.RunnableBinding{}
	if err = protojson.Unmarshal(prepared.Package, pkg); err != nil {
		t.Fatal(err)
	}
	if err = protojson.Unmarshal(prepared.Binding, binding); err != nil {
		t.Fatal(err)
	}
	if err = corerunnable.VerifyBinding(binding, pkg); err != nil {
		t.Fatal("binding does not verify against its package: ", err)
	}
	if prepared.Operation == nil || prepared.Operation.Method != applyTextMethod {
		t.Fatalf("operation = %+v, want %s", prepared.Operation, applyTextMethod)
	}
	descriptors, err := base64.StdEncoding.DecodeString(prepared.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	if err = googleproto.Unmarshal(descriptors, set); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range set.GetFile() {
		names = append(names, file.GetName())
	}
	if !strings.Contains(strings.Join(names, " "), "documents/ingest/v1/ingestion.proto") {
		t.Fatalf("descriptors %v do not carry the second package's service file", names)
	}
	if strings.Contains(strings.Join(names, " "), "acme/documents/v1/api.proto") {
		t.Fatalf("descriptors %v carry the primary package's file, which the method does not need", names)
	}
}

// Two services of one endpoint that both mark a method of one name would
// derive the same directory; the second is refused by name instead of
// silently overwriting the first.
func TestGenerateRunnablesRefusesTwoMethodsDerivingOneDirectory(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	// A conforming operation of DocumentService's own: its receipt lookup is
	// on the same service, as the derivation requires.
	operation := conformingOperation()
	operation.LookupMethod = "/acme.documents.v1.DocumentService/LookupText"
	publishMultiPackageContract(t, moduleDir, multiPackageContract(t, documentsFile(
		markedMethod("ApplyText", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", operation),
		markedMethod("LookupText", ".documents.ingest.v1.LookupTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil),
	)))

	t.Chdir(root)
	resetRunnablesFlags(t)
	err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"})
	if err == nil {
		t.Fatal("two methods deriving one directory were accepted")
	}
	for _, want := range []string{"/acme.documents.v1.DocumentService/ApplyText", "runtime-worker/grpc/ApplyText", applyTextMethod} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal = %v, want it to name %q", err, want)
		}
	}
}
