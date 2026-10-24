package gitops

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	delivery "github.com/codefly-dev/authority/libraries/delivery/go"
	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
)

// policyPackage writes a module package whose one protobuf contract serves
// WhoAmI (requires console.identity read), Version (any checked caller) and
// Plain (no mark), and returns its directory.
func policyPackage(t *testing.T, tamper bool) string {
	t.Helper()
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String("ApiService")}
	marks := map[string]*hopv1.Policy{
		"WhoAmI":  {Requires: []*hopv1.Requirement{{ResourceKind: "console.identity", Actions: []string{"read"}}}},
		"Version": {AnyCheckedCaller: true},
		"Plain":   nil,
	}
	for _, name := range []string{"WhoAmI", "Version", "Plain"} {
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(".api.Empty"), OutputType: proto.String(".api.Empty")}
		if marks[name] != nil {
			m.Options = &descriptorpb.MethodOptions{}
			proto.SetExtension(m.Options, hopv1.E_Policy, marks[name])
		}
		service.Method = append(service.Method, m)
	}
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("api.proto"), Package: proto.String("api"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}}, Service: []*descriptorpb.ServiceDescriptorProto{service},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	contract := filepath.Join(dir, "contracts", "api", "api", "grpc", "contract.binpb")
	for _, root := range []string{filepath.Dir(contract), filepath.Join(dir, "services")} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(contract, set, 0o644); err != nil {
		t.Fatal(err)
	}
	if tamper {
		set = append(set, 0)
	}
	sum := sha256.Sum256(set)
	pkg := `schema: codefly/module-package/v2
kind: module-package
id: codefly/console
version: 0.1.0
minimum-codefly-version: ">=0.3.32 <1.0.0"
artifact-roots:
  - contracts
  - services
services:
  - name: api
    endpoints: [grpc]
    api-contracts:
      - endpoint: grpc
        kind: protobuf
        package: api
        path: contracts/api/api/grpc/contract.binpb
        digest: sha256:` + hex.EncodeToString(sum[:]) + `
contracts:
  composition: ">=2.0.0 <3.0.0"
`
	if err := os.WriteFile(filepath.Join(dir, "module.package.codefly.yaml"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMethodPoliciesAreReadFromThePublishedContract(t *testing.T) {
	methods, ok, err := MethodPolicies(policyPackage(t, false))
	if err != nil || !ok || len(methods) != 3 {
		t.Fatalf("methods: %v %v %v", methods, ok, err)
	}
	byName := map[string]*compositionv1.MethodPolicyV1{}
	for _, m := range methods {
		byName[m.GetMethod()] = m
	}
	if r := byName["/api.ApiService/WhoAmI"].GetRequires(); len(r) != 1 || r[0].GetResourceKind() != "console.identity" || r[0].GetActions()[0] != "read" {
		t.Fatalf("WhoAmI: %v", byName["/api.ApiService/WhoAmI"])
	}
	if !byName["/api.ApiService/Version"].GetAnyCheckedCaller() {
		t.Fatal("Version admits any checked caller")
	}
	if p := byName["/api.ApiService/Plain"]; p == nil || p.GetAnyCheckedCaller() || len(p.GetRequires()) != 0 {
		t.Fatalf("an unmarked method is listed with nothing set: %v", p)
	}
	if _, _, err := MethodPolicies(policyPackage(t, true)); err == nil || !strings.Contains(err.Error(), "the package names") {
		t.Fatalf("a contract that is not its digest: %v", err)
	}
	if _, ok, err := MethodPolicies(t.TempDir()); ok || err != nil {
		t.Fatalf("a module with no package declares no method policy: %v %v", ok, err)
	}
}

func TestTheAuthorityDocumentIsSignedAndBoundToItsManifest(t *testing.T) {
	stub := &compositionStub{held: map[string]uint64{}}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, stub)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(nil)
	signer, err := solutionhost.NewInMemorySigner("release", private)
	if err != nil {
		t.Fatal(err)
	}
	want := []*compositionv1.MethodPolicyV1{{Method: "/api.ApiService/WhoAmI", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "console.identity", Actions: []string{"read"}}}}}
	ceilings := []*authorityv1.WorkScopeV1{{ResourceKind: "console.identity", Actions: []string{"read"}}}
	d := NewCompositionDelivery(conn, signer, func(_ context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		return want, module == "console", nil
	}, func(_ context.Context, module string) ([]*authorityv1.WorkScopeV1, error) {
		return ceilings, nil
	})
	manifest := &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "console", Alias: "console", Kind: solutionhost.KindModule, Generation: 3,
		Host: solutionhost.HostTarget{Coordinate: "local", Domain: "local"}, Release: solutionhost.Release{Publisher: "test", Name: "console", Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}}, Authority: &solutionhost.Authority{Principal: "console"},
	}
	document, err := solutionhost.SignedPayload(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Post(context.Background(), "console", &solutionhost.Signed{Document: document, Envelope: []byte(`{}`)}, "delivery-credential"); err != nil {
		t.Fatal(err)
	}
	authority := stub.seen[0].GetAuthority()
	if _, err := delivery.Verify(authority.GetDocument(), authority.GetEnvelope(), map[string]crypto.PublicKey{"release": public}); err != nil {
		t.Fatalf("the document is signed by the run's key: %v", err)
	}
	var doc compositionv1.AuthorityManifestV1
	if err := proto.Unmarshal(authority.GetDocument(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.GetModule() != "console" || doc.GetGeneration() != 3 || doc.GetCoreManifestDigest() != delivery.Digest(document) || !proto.Equal(doc.GetMethods()[0], want[0]) {
		t.Fatalf("the document names its manifest and carries the methods: %v", &doc)
	}
	// And the module's own kinds with their actions, which core's manifest
	// names without them.
	if len(doc.GetScopeCeilings()) != 1 || !proto.Equal(doc.GetScopeCeilings()[0], ceilings[0]) {
		t.Fatalf("the document carries the scope ceilings: %v", doc.GetScopeCeilings())
	}
}

// A module's scope ceilings come from the contract it publishes, kind by kind
// with their actions; a module publishing none declares none.
func TestScopeCeilingsAreReadFromTheModuleContract(t *testing.T) {
	dir := t.TempDir()
	if got, err := ScopeCeilings(dir); err != nil || got != nil {
		t.Fatalf("no contract: %v %v", got, err)
	}
	contract := "schema: codefly/module-contract/v1\nprincipal: runtime\nnamespaces: []\nqueues: []\nscope_ceilings:\n    - resource_kind: runtime.tasks\n      actions: [execute, start]\nbindings: []\ndestinations: []\n"
	if err := os.WriteFile(filepath.Join(dir, "module.contract.codefly.yaml"), []byte(contract), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ScopeCeilings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GetResourceKind() != "runtime.tasks" || !slices.Equal(got[0].GetActions(), []string{"execute", "start"}) {
		t.Fatalf("ceilings: %v", got)
	}
}
