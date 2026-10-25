package gitops

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/solutionhost"
	"google.golang.org/genproto/googleapis/api/annotations"
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
		if name == "WhoAmI" {
			// Bound to HTTP too: the route rows carry the method's policy.
			proto.SetExtension(m.Options, annotations.E_Http, &annotations.HttpRule{
				Pattern: &annotations.HttpRule_Get{Get: "/v1/console/whoami"},
				AdditionalBindings: []*annotations.HttpRule{{Pattern: &annotations.HttpRule_Post{Post: "/v1/console/identities/{id=**}:whoami"}},
					{Pattern: &annotations.HttpRule_Post{Post: "/v1/console/{ref=reviews/*}:settle"}}}})
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
	if err != nil || !ok || len(methods) != 6 {
		t.Fatalf("methods: %v %v %v", methods, ok, err)
	}
	byName := map[string]*compositionv1.MethodPolicyV1{}
	for _, m := range methods {
		byName[m.GetMethod()] = m
	}
	for _, name := range []string{"/api.ApiService/WhoAmI", "GET /v1/console/whoami", "POST /v1/console/identities/{id=**}:whoami", "POST /v1/console/reviews/{ref}:settle"} {
		if r := byName[name].GetRequires(); len(r) != 1 || r[0].GetResourceKind() != "console.identity" || r[0].GetActions()[0] != "read" {
			t.Fatalf("%s: %v", name, byName[name])
		}
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
	roles := []*compositionv1.RoleV1{{Name: modulecontract.InstallerRole, Scopes: ceilings}}
	d := NewCompositionDelivery(conn, signer, func(_ context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error) {
		return want, module == "console", nil
	}, func(_ context.Context, module string) ([]*authorityv1.WorkScopeV1, error) {
		return ceilings, nil
	}, func(_ context.Context, module string) ([]*compositionv1.RoleV1, error) {
		return roles, nil
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
	if _, err := send(context.Background(), d, "console", &solutionhost.Signed{Document: document, Envelope: []byte(`{}`)}, "delivery-credential"); err != nil {
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
	// And a solution's roles, which core's manifest does not carry.
	if len(doc.GetRoles()) != 1 || !proto.Equal(doc.GetRoles()[0], roles[0]) {
		t.Fatalf("the document carries the roles: %v", doc.GetRoles())
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

// routePackage is a module package whose one service publishes an OpenAPI
// contract, each operation's policy in its x-codefly-authority-policy.
func routePackage(t *testing.T, document string) string {
	t.Helper()
	dir := t.TempDir()
	contract := filepath.Join(dir, "contracts", "api", "model", "rest", "openapi.json")
	for _, root := range []string{filepath.Dir(contract), filepath.Join(dir, "services")} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(contract, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(document))
	pkg := `schema: codefly/module-package/v2
kind: module-package
id: obin/model
version: 0.1.0
minimum-codefly-version: ">=0.3.32 <1.0.0"
artifact-roots:
  - contracts
  - services
services:
  - name: model
    endpoints: [rest]
    api-contracts:
      - endpoint: rest
        kind: openapi
        package: ModelService
        path: contracts/api/model/rest/openapi.json
        digest: sha256:` + hex.EncodeToString(sum[:]) + `
contracts:
  composition: ">=2.0.0 <3.0.0"
`
	if err := os.WriteFile(filepath.Join(dir, "module.package.codefly.yaml"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A module serving HTTP declares its routes' policies in its OpenAPI contract:
// each operation is "<VERB> <path>", read with the policy its extension
// declares, the same message a proto method marks itself with; an operation
// declaring nothing is listed with nothing set, and an extension that is not a
// policy is refused by name.
func TestRoutePoliciesAreReadFromThePublishedOpenAPIContract(t *testing.T) {
	methods, ok, err := MethodPolicies(routePackage(t, `{"openapi":"3.1.0","info":{"title":"ModelService"},"paths":{
		"/v1/model/profiles":{"get":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"modelservice.profiles","actions":["read"]}]}}},
		"/v1/model/profiles/{ref}":{"parameters":[],"get":{"x-codefly-authority-policy":{"any_checked_caller":true}},"delete":{}}}}`))
	if err != nil || !ok || len(methods) != 3 {
		t.Fatalf("methods: %v %v %v", methods, ok, err)
	}
	byName := map[string]*compositionv1.MethodPolicyV1{}
	for _, m := range methods {
		byName[m.GetMethod()] = m
	}
	if r := byName["GET /v1/model/profiles"].GetRequires(); len(r) != 1 || r[0].GetResourceKind() != "modelservice.profiles" || r[0].GetActions()[0] != "read" {
		t.Fatalf("GET /v1/model/profiles: %v", byName["GET /v1/model/profiles"])
	}
	if !byName["GET /v1/model/profiles/{ref}"].GetAnyCheckedCaller() {
		t.Fatal("the profile read admits any checked caller")
	}
	if p := byName["DELETE /v1/model/profiles/{ref}"]; p == nil || p.GetAnyCheckedCaller() || len(p.GetRequires()) != 0 {
		t.Fatalf("an unmarked operation is listed with nothing set: %v", p)
	}
	_, _, err = MethodPolicies(routePackage(t, `{"paths":{"/v1/x":{"get":{"x-codefly-authority-policy":{"requires":"everything"}}}}}`))
	if err == nil || !strings.Contains(err.Error(), "authority.hop.v1.Policy") {
		t.Fatalf("an extension that is not a policy: %v", err)
	}
}

// A module serving a protocol it does not own declares those methods in its
// module contract; they are delivered beside its published methods, with what
// each requires, and one that a published contract also serves is refused.
func TestForeignMethodsJoinThePublishedOnes(t *testing.T) {
	dir := policyPackage(t, false)
	contract := `schema: codefly/module-contract/v1
principal: console
namespaces: []
queues: []
scope_ceilings:
    - resource_kind: console.spans
      actions: [export]
bindings: []
destinations: []
foreign_methods:
    - method: %s
      requires:
          - resource_kind: console.spans
            actions: [export]
`
	write := func(method string) {
		if err := os.WriteFile(filepath.Join(dir, modulecontract.FileName), []byte(fmt.Sprintf(contract, method)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("/opentelemetry.proto.collector.trace.v1.TraceService/Export")
	methods, ok, err := MethodPolicies(dir)
	if err != nil || !ok {
		t.Fatalf("methods: %v %v", ok, err)
	}
	byName := map[string]*compositionv1.MethodPolicyV1{}
	for _, m := range methods {
		byName[m.GetMethod()] = m
	}
	export := byName["/opentelemetry.proto.collector.trace.v1.TraceService/Export"]
	if export == nil || len(export.GetRequires()) != 1 || export.GetRequires()[0].GetResourceKind() != "console.spans" || export.GetRequires()[0].GetActions()[0] != "export" {
		t.Fatalf("the foreign Export: %v", export)
	}
	if byName["/api.ApiService/WhoAmI"] == nil {
		t.Fatal("the published methods are still delivered")
	}
	write("/api.ApiService/WhoAmI")
	if _, _, err := MethodPolicies(dir); err == nil || !strings.Contains(err.Error(), "already serve") {
		t.Fatalf("a foreign method the package publishes: %v", err)
	}
}

// What delivery lists is what the module serves: a method the descriptor
// carries but the catalog records no endpoint serving is left out, routes
// and all, and named as unserved; a catalog naming another digest, or no
// entry for the endpoint, is stale and refused.
func TestMethodPoliciesListOnlyTheServedMethods(t *testing.T) {
	dir := policyPackage(t, false)
	manifest, err := composition.LoadPackageManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	contract := manifest.Services[0].APIContracts[0]
	writeCatalog := func(digest string, endpoints ...string) {
		catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: manifest.ID, Version: manifest.Version}
		for _, endpoint := range endpoints {
			catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{
				Service: "api", Endpoint: endpoint, API: "grpc", Kind: contract.Kind, Package: contract.Package, Path: contract.Path, Digest: digest,
				Services: []composition.APIContractService{{Name: "ApiService", FullName: "api.ApiService", Procedures: []string{"/api.ApiService/WhoAmI", "/api.ApiService/Version"}}},
			})
		}
		data, err := catalog.CanonicalBytes()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(composition.APIContractCatalogFileName))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeCatalog(contract.Digest, "grpc")
	served, ok, err := ServiceMethodPolicies(dir)
	if err != nil || !ok {
		t.Fatalf("served: %v %v", ok, err)
	}
	if !slices.Equal(served.Unserved, []string{"/api.ApiService/Plain"}) {
		t.Fatalf("unserved: %v", served.Unserved)
	}
	methods := served.All()
	var names []string
	for _, m := range methods {
		names = append(names, m.GetMethod())
	}
	if len(methods) != 5 || slices.Contains(names, "/api.ApiService/Plain") || !slices.Contains(names, "GET /v1/console/whoami") {
		t.Fatalf("served methods: %v", names)
	}
	writeCatalog("sha256:"+strings.Repeat("0", 64), "grpc")
	if _, _, err := MethodPolicies(dir); err == nil || !strings.Contains(err.Error(), "run codefly generate contracts") {
		t.Fatalf("a catalog naming another digest: %v", err)
	}
	writeCatalog(contract.Digest, "connect")
	if _, _, err := MethodPolicies(dir); err == nil || !strings.Contains(err.Error(), "records no endpoint api/grpc") {
		t.Fatalf("a catalog with no entry for the endpoint: %v", err)
	}
}
