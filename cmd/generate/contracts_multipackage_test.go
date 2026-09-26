package generate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/standards"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// ownProtoDir lays out the given files as a service's own proto sources, which
// is how serviceContractPackages tells them apart from buf's imports.
func ownProtoDir(t *testing.T, files ...string) string {
	t.Helper()
	protoDir := t.TempDir()
	for _, rel := range files {
		writeFixtureFile(t, filepath.Join(protoDir, filepath.FromSlash(rel)), []byte("syntax = \"proto3\";\n"))
	}
	return protoDir
}

func contractService(name string, methods ...string) *descriptorpb.ServiceDescriptorProto {
	service := &descriptorpb.ServiceDescriptorProto{Name: googleproto.String(name)}
	for _, method := range methods {
		service.Method = append(service.Method, &descriptorpb.MethodDescriptorProto{Name: googleproto.String(method)})
	}
	return service
}

func contractFile(name, pkg string, services ...*descriptorpb.ServiceDescriptorProto) *descriptorpb.FileDescriptorProto {
	return &descriptorpb.FileDescriptorProto{Name: googleproto.String(name), Package: googleproto.String(pkg), Service: services}
}

// multiPackageSet is a service that serves its own API beside a generic
// receipt service in another package, plus an import that declares a service
// the endpoint does not serve.
func multiPackageSet() *descriptorpb.FileDescriptorSet {
	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		contractFile("google/longrunning/operations.proto", "google.longrunning", contractService("Operations", "GetOperation")),
		contractFile("acme/documents/v1/api.proto", "acme.documents.v1", contractService("DocumentService", "Get", "Put")),
		contractFile("acme/receipts/v1/receipts.proto", "acme.receipts.v1", contractService("ReceiptService", "Lookup")),
	}}
}

func TestProtobufContractSurfaceRecordsEveryPackagesServices(t *testing.T) {
	protoDir := ownProtoDir(t, "acme/documents/v1/api.proto", "acme/receipts/v1/receipts.proto")

	pkg, services, err := protobufContractSurface(multiPackageSet(), protoDir, "acme.documents.v1")
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "acme.documents.v1" {
		t.Fatalf("package = %q, want the declared primary acme.documents.v1", pkg)
	}
	var got []string
	for _, service := range services {
		got = append(got, service.FullName+"="+strings.Join(service.Procedures, ","))
	}
	want := []string{
		"acme.documents.v1.DocumentService=/acme.documents.v1.DocumentService/Get,/acme.documents.v1.DocumentService/Put",
		"acme.receipts.v1.ReceiptService=/acme.receipts.v1.ReceiptService/Lookup",
	}
	if !equalStrings(got, want) {
		t.Fatalf("services = %v, want %v (and never the import's google.longrunning.Operations)", got, want)
	}

	// The entry is a valid catalog entry: core's schema already allows
	// services outside Package, so no schema change is needed.
	catalog := &composition.APIContractCatalog{
		Schema: composition.APIContractCatalogSchema, Package: "codefly/documents", Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service: "documents", Endpoint: "grpc", API: "grpc", Kind: composition.APIContractKindProtobuf,
			Package: pkg, Path: "contracts/api/documents/grpc/contract.binpb", Digest: composition.APIContractDigest([]byte("x")), Services: services,
		}},
	}
	if err = catalog.Validate(); err != nil {
		t.Fatalf("a multi-package entry does not validate: %v", err)
	}
}

func TestProtobufContractSurfaceRefusesAnUndeclaredPrimary(t *testing.T) {
	protoDir := ownProtoDir(t, "acme/documents/v1/api.proto", "acme/receipts/v1/receipts.proto")
	for _, declared := range []string{"", "acme.other.v1"} {
		_, _, err := protobufContractSurface(multiPackageSet(), protoDir, declared)
		if err == nil {
			t.Fatalf("declared %q: a multi-package endpoint was given a primary package its agent does not declare", declared)
		}
		if !strings.Contains(err.Error(), "[acme.documents.v1 acme.receipts.v1]") || !strings.Contains(err.Error(), "primary package") {
			t.Fatalf("declared %q: error = %v, want it to name both packages and the primary-package rule", declared, err)
		}
	}
}

func TestProtobufContractSurfaceRefusesAShortNameClash(t *testing.T) {
	protoDir := ownProtoDir(t, "acme/documents/v1/api.proto", "acme/receipts/v1/receipts.proto")
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		contractFile("acme/documents/v1/api.proto", "acme.documents.v1", contractService("Service", "Get")),
		contractFile("acme/receipts/v1/receipts.proto", "acme.receipts.v1", contractService("Service", "Lookup")),
	}}
	_, _, err := protobufContractSurface(set, protoDir, "acme.documents.v1")
	if err == nil || !strings.Contains(err.Error(), "acme.documents.v1.Service and acme.receipts.v1.Service") {
		t.Fatalf("error = %v, want the two same-named services refused by their full names", err)
	}
}

// TestProtobufContractSurfaceSinglePackageCatalogIsUnchanged holds a
// single-package export to the bytes it produced before multi-package
// endpoints existed. The golden was written by the previous code
// (serviceContractPackage + composition.ProtobufServices) over this same set;
// the declared package is ignored when there is only one.
func TestProtobufContractSurfaceSinglePackageCatalogIsUnchanged(t *testing.T) {
	protoDir := ownProtoDir(t, "acme/billing/v1/api.proto", "acme/billing/v1/admin.proto", "acme/shared/v1/types.proto")
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
		contractFile("google/longrunning/operations.proto", "google.longrunning", contractService("Operations", "GetOperation")),
		contractFile("acme/shared/v1/types.proto", "acme.shared.v1"),
		contractFile("acme/billing/v1/api.proto", "acme.billing.v1", contractService("InvoiceService", "Void", "Create")),
		contractFile("acme/billing/v1/admin.proto", "acme.billing.v1", contractService("AdminService", "Reset")),
	}}
	contract, err := googleproto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}

	for _, declared := range []string{"", "acme.billing.v1", "acme.unrelated.v1"} {
		pkg, services, err := protobufContractSurface(set, protoDir, declared)
		if err != nil {
			t.Fatalf("declared %q: %v", declared, err)
		}
		catalog := &composition.APIContractCatalog{
			Schema: composition.APIContractCatalogSchema, Package: "codefly/billing", Version: "0.1.0",
			Endpoints: []composition.APIContractEndpoint{{
				Service: "api", Endpoint: "grpc", API: "grpc", Kind: composition.APIContractKindProtobuf,
				Package: pkg, Path: "contracts/api/api/grpc/contract.binpb", Digest: composition.APIContractDigest(contract), Services: services,
			}},
		}
		got, err := catalog.CanonicalBytes()
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", "contracts-single-package.catalog.golden.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("declared %q: single-package catalog changed:\n%s\nwant:\n%s", declared, got, want)
		}
	}
}

// A connect endpoint carries no GrpcAPI; it takes the primary package from
// the service's grpc endpoint, which serves the same proto.
func TestDeclaredProtoPackage(t *testing.T) {
	ctx := context.Background()
	grpcEndpoint := func(name, pkg string) *basev0.Endpoint {
		return &basev0.Endpoint{Name: name, Api: standards.GRPC, ApiDetails: &basev0.API{Value: &basev0.API_Grpc{Grpc: &basev0.GrpcAPI{Package: pkg}}}}
	}
	connect := &basev0.Endpoint{Name: "connect", Api: standards.CONNECT}
	own := grpcEndpoint("grpc", "acme.documents.v1")

	if got := declaredProtoPackage(ctx, own, []*basev0.Endpoint{own, connect}); got != "acme.documents.v1" {
		t.Fatalf("grpc endpoint: %q", got)
	}
	if got := declaredProtoPackage(ctx, connect, []*basev0.Endpoint{connect, own}); got != "acme.documents.v1" {
		t.Fatalf("connect endpoint: %q, want its grpc sibling's package", got)
	}
	if got := declaredProtoPackage(ctx, connect, []*basev0.Endpoint{connect}); got != "" {
		t.Fatalf("connect endpoint without a grpc sibling: %q, want none", got)
	}
	disagreeing := []*basev0.Endpoint{connect, own, grpcEndpoint("admin", "acme.admin.v1")}
	if got := declaredProtoPackage(ctx, connect, disagreeing); got != "" {
		t.Fatalf("grpc siblings that disagree: %q, want none", got)
	}
}
