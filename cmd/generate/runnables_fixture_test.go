package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// The derivation reads descriptors, so the fixtures are descriptors. Building
// them here rather than checking in a compiled set keeps an out-of-profile
// method three lines away from a conforming one, and keeps the suite free of a
// generated artifact nothing regenerates.

const (
	ingestPackage   = "documents.ingest.v1"
	applyTextMethod = "/documents.ingest.v1.IngestionService/ApplyText"
	streamMethod    = "/documents.ingest.v1.IngestionService/StreamText"
)

func stringField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   googleproto.String(name),
		Number: googleproto.Int32(number),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
	}
}

func boolField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	field := stringField(name, number)
	field.Type = descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()
	return field
}

func int64Field(name string, number int32) *descriptorpb.FieldDescriptorProto {
	field := stringField(name, number)
	field.Type = descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	return field
}

// uint64Field is outside the bounded profile: its range exceeds the signed
// 64-bit integer the profile carries.
func uint64Field(name string, number int32) *descriptorpb.FieldDescriptorProto {
	field := stringField(name, number)
	field.Type = descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()
	return field
}

func descriptorMessage(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
	return &descriptorpb.DescriptorProto{Name: googleproto.String(name), Field: fields}
}

// conformingOperation is the policy document-store installs for its text
// operation, written as the option the method carries.
func conformingOperation() *runnablev0.Operation {
	return &runnablev0.Operation{
		AttemptTimeout: durationpb.New(10 * time.Second),
		TotalTimeout:   durationpb.New(time.Minute),
		MaxAttempts:    1,
		Backoff:        durationpb.New(time.Second),
		RetryableCodes: []string{"UNAVAILABLE"},
		Audience:       "documents.ingestion",
		InvokeScopes:   []*basev0.WorkScopeV1{{ResourceKind: "documents", Actions: []string{"ingest", "read"}}},
		LookupScopes:   []*basev0.WorkScopeV1{{ResourceKind: "documents", Actions: []string{"read"}}},
		LookupMethod:   "/documents.ingest.v1.IngestionService/LookupText",
	}
}

func markedMethod(name, input, output string, declared *runnablev0.Operation) *descriptorpb.MethodDescriptorProto {
	method := &descriptorpb.MethodDescriptorProto{
		Name:       googleproto.String(name),
		InputType:  googleproto.String(input),
		OutputType: googleproto.String(output),
	}
	if declared != nil {
		method.Options = &descriptorpb.MethodOptions{}
		googleproto.SetExtension(method.Options, runnablev0.E_Operation, declared)
	}
	return method
}

// ingestionFile mirrors module-document-store's published ingestion schema,
// whose hand-written Runnable package is the oracle this derivation has to
// reproduce. extra methods are appended to the service as given.
func ingestionFile(declared *runnablev0.Operation, extra ...*descriptorpb.MethodDescriptorProto) *descriptorpb.FileDescriptorProto {
	methods := []*descriptorpb.MethodDescriptorProto{
		markedMethod("ApplyText", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", declared),
		// LookupText is the receipt method ApplyText's policy names. It is
		// unmarked, so it is not an operation of its own.
		markedMethod("LookupText", ".documents.ingest.v1.LookupTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil),
	}
	methods = append(methods, extra...)
	return &descriptorpb.FileDescriptorProto{
		Name:       googleproto.String("documents/ingest/v1/ingestion.proto"),
		Package:    googleproto.String(ingestPackage),
		Syntax:     googleproto.String("proto3"),
		Dependency: []string{"codefly/runnable/v0/options.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			descriptorMessage("ApplyTextRequest",
				stringField("origin", 1),
				stringField("container", 2),
				stringField("ref", 3),
				stringField("path", 4),
				stringField("revision", 5),
				stringField("commit", 6),
				int64Field("cursor", 7),
				stringField("content_type", 8),
				stringField("text", 9),
			),
			descriptorMessage("ApplyTextResponse",
				stringField("reference", 1),
				stringField("content_hash", 2),
				stringField("effect_digest", 3),
				boolField("minted", 4),
				boolField("skipped", 5),
				boolField("quarantined", 6),
			),
			descriptorMessage("LookupTextRequest", stringField("effect_digest", 1)),
			// CountRequest is deliberately out of the bounded profile.
			descriptorMessage("CountRequest", uint64Field("total", 1)),
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name:   googleproto.String("IngestionService"),
			Method: methods,
		}},
	}
}

// outOfProfileMethod takes a uint64 the bounded profile has no type for. It is
// marked, so it is refused rather than skipped.
func outOfProfileMethod() *descriptorpb.MethodDescriptorProto {
	return markedMethod("CountText", ".documents.ingest.v1.CountRequest", ".documents.ingest.v1.ApplyTextResponse", conformingOperation())
}

// streamingMethod is marked and streams, which no Runnable operation may.
func streamingMethod() *descriptorpb.MethodDescriptorProto {
	method := markedMethod("StreamText", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", conformingOperation())
	method.ServerStreaming = googleproto.Bool(true)
	return method
}

// descriptorSet serializes the fixture the way `codefly generate contracts`
// writes a contract: a FileDescriptorSet carrying the file and, ahead of it,
// the transitive closure of what it imports. buf emits that closure, and the
// derivation resolves the set as a whole, so a fixture without it would test a
// shape the command never sees.
func descriptorSet(t *testing.T, file *descriptorpb.FileDescriptorProto) []byte {
	t.Helper()
	files := map[string]bool{}
	var closure []*descriptorpb.FileDescriptorProto
	var collect func(path string)
	collect = func(path string) {
		if files[path] {
			return
		}
		files[path] = true
		imported, err := protoregistry.GlobalFiles.FindFileByPath(path)
		if err != nil {
			t.Fatal(err)
		}
		descriptor := protodesc.ToFileDescriptorProto(imported)
		for _, dependency := range descriptor.GetDependency() {
			collect(dependency)
		}
		closure = append(closure, descriptor)
	}
	for _, dependency := range file.GetDependency() {
		collect(dependency)
	}

	data, err := googleproto.Marshal(&descriptorpb.FileDescriptorSet{File: append(closure, file)})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// saveRunnableFixture writes a module whose service publishes one protobuf
// endpoint, with the contract catalog `generate contracts` would have left
// behind. It returns the workspace root and the module directory.
func saveRunnableFixture(t *testing.T, ctx context.Context, contract []byte, manifestVersion string) (root, moduleDir string) {
	t.Helper()
	root, moduleDir = saveFixtureWorkspace(t, ctx, "documents",
		&resources.ModuleInterface{
			Endpoints: []*resources.InterfaceEndpoint{
				{Service: "runtime-worker", Endpoint: "grpc", Visibility: resources.VisibilityPublic},
			},
		},
		&resources.Service{
			Name:    "runtime-worker",
			Version: "0.0.1",
			Agent:   &resources.Agent{Kind: resources.ServiceAgent, Name: "go", Version: "0.0.48", Publisher: "codefly.dev"},
			Endpoints: []*resources.Endpoint{
				{Name: "grpc", API: "grpc", Visibility: resources.VisibilityPublic},
			},
		},
	)

	publishContract(t, moduleDir, contract)

	if manifestVersion != "" {
		writeFixtureFile(t, filepath.Join(moduleDir, composition.PackageManifestFileName), []byte(
			"kind: module-package\nschema: codefly/module-package/v2\nid: codefly/documents\nversion: "+manifestVersion+
				"\nminimum-codefly-version: \">=0.1.0\"\ncontracts:\n  composition: \">=2.0 <3.0\"\nartifact-roots:\n  - contracts\nservices:\n  - name: runtime-worker\n    endpoints:\n      - grpc\n"))
	}
	return root, moduleDir
}

// publishContract writes one endpoint's contract and the catalog that names
// it, the way `codefly generate contracts` leaves them behind.
func publishContract(t *testing.T, moduleDir string, contract []byte) {
	t.Helper()
	const contractPath = "contracts/api/runtime-worker/grpc/contract.binpb"
	writeFixtureFile(t, filepath.Join(moduleDir, filepath.FromSlash(contractPath)), contract)

	catalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/documents",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "runtime-worker",
			Endpoint: "grpc",
			API:      "grpc",
			Kind:     composition.APIContractKindProtobuf,
			Package:  ingestPackage,
			Path:     contractPath,
			Digest:   composition.APIContractDigest(contract),
			Services: composition.ProtobufServices(mustParseSet(t, contract), ingestPackage),
		}},
	}
	catalogBytes, err := catalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(moduleDir, filepath.FromSlash(composition.APIContractCatalogFileName)), catalogBytes)
}

func mustParseSet(t *testing.T, data []byte) *descriptorpb.FileDescriptorSet {
	t.Helper()
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(data, &set); err != nil {
		t.Fatal(err)
	}
	return &set
}

func writeFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
