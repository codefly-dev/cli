package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/composition"
	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// sandboxDescriptor is a service that registers three methods, the shape of a
// code sandbox that gained a mounted run: its inventory was not regenerated
// with it.
func sandboxDescriptor(t *testing.T) []byte {
	t.Helper()
	file := &descriptorpb.FileDescriptorProto{
		Name: googleproto.String("fixture/sandbox/v1/api.proto"), Package: googleproto.String("fixture.sandbox.v1"),
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: googleproto.String("SandboxService")}},
	}
	for _, method := range []string{"Run", "RunMounted", "Version"} {
		file.Service[0].Method = append(file.Service[0].Method, &descriptorpb.MethodDescriptorProto{Name: googleproto.String(method)})
	}
	data, err := googleproto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{file}})
	require.NoError(t, err)
	return data
}

func sandboxSurface(methods ...string) []composition.APIContractService {
	service := composition.APIContractService{Name: "SandboxService", FullName: "fixture.sandbox.v1.SandboxService"}
	for _, method := range methods {
		service.Procedures = append(service.Procedures, "/fixture.sandbox.v1.SandboxService/"+method)
	}
	return []composition.APIContractService{service}
}

// writeSandboxListeners writes the grpc and connect contracts of the sandbox
// with the given inventory, as generate contracts writes them, and returns the
// catalog rows and the directory they were written under.
func writeSandboxListeners(t *testing.T, surface []composition.APIContractService) ([]composition.APIContractEndpoint, string) {
	t.Helper()
	ctx := context.Background()
	module := &resources.Module{Name: "assistant"}
	module.WithDir(t.TempDir())
	service := &resources.Service{Name: "sandbox"}
	service.WithDir(filepath.Join(module.Dir(), "services/sandbox"))
	writeFixtureFile(t, filepath.Join(service.Dir(), "proto/fixture/sandbox/v1/api.proto"), []byte("// source snapshot\n"))
	descriptor := sandboxDescriptor(t)
	out := filepath.Join(module.Dir(), "contracts/api")
	var rows []composition.APIContractEndpoint
	for _, name := range []string{"grpc", "connect"} {
		dir := filepath.Join(out, service.Name, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		entry, err := writeBuiltProtobufContract(ctx, module, service, &basev1.Endpoint{Name: name, Api: name}, surface, dir, dir, descriptor)
		require.NoError(t, err, "each listener alone names only methods its descriptor has")
		rows = append(rows, *entry)
	}
	return rows, out
}

func readListener(dir string) func(composition.APIContractEndpoint) ([]byte, error) {
	return func(endpoint composition.APIContractEndpoint) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, endpoint.Service, endpoint.Endpoint, "contract.binpb"))
	}
}

// The inventory that shipped without RunMounted passed every check generate
// contracts ran — each listener names only methods its descriptor has — and was
// refused only when a composition that calls RunMounted was rendered.
func TestCatalogCoverageRefusesAnInventoryMissingAServedMethod(t *testing.T) {
	rows, dir := writeSandboxListeners(t, sandboxSurface("Run", "Version"))
	err := ProtobufCatalogCoverage(rows, readListener(dir))
	require.ErrorContains(t, err, "/fixture.sandbox.v1.SandboxService/RunMounted")
	require.ErrorContains(t, err, "service sandbox")
}

func TestCatalogCoverageAcceptsTheServedSurface(t *testing.T) {
	rows, dir := writeSandboxListeners(t, sandboxSurface("Run", "RunMounted", "Version"))
	require.NoError(t, ProtobufCatalogCoverage(rows, readListener(dir)))
}

// A restricted listener may serve part of a service when a sibling sharing its
// descriptor serves the rest.
func TestCatalogCoverageAcceptsListenersThatSplitTheService(t *testing.T) {
	rows, dir := writeSandboxListeners(t, sandboxSurface("Run", "RunMounted", "Version"))
	rows[1].Services = sandboxSurface("Version")
	require.NoError(t, ProtobufCatalogCoverage(rows, readListener(dir)))
	rows[0].Services = sandboxSurface("Run")
	require.ErrorContains(t, ProtobufCatalogCoverage(rows, readListener(dir)), "RunMounted")
}
