package generate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"google.golang.org/protobuf/reflect/protodesc"
	"os"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"google.golang.org/protobuf/encoding/protojson"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func resetRunnableBindingsFlags(t *testing.T) {
	t.Helper()
	runnableBindingsEnv, runnableBindingsCheck = "local", false
	t.Cleanup(func() { runnableBindingsEnv, runnableBindingsCheck = "local", false })
}

// Every derived operation is bound to its owner endpoint at the address the
// environment resolves, verified against its package, with the policy the
// method declared and the owner's descriptors for the method; --check holds
// the committed file to that.
func TestGenerateRunnableBindingsBindsEveryDerivedOperation(t *testing.T) {
	ctx := context.Background()
	root, _ := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0")
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	if err := RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}); err != nil {
		t.Fatal(err)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "#") {
		t.Fatalf("want a header and one binding:\n%s", raw)
	}
	key, value, _ := strings.Cut(lines[1], "=")
	if !strings.HasPrefix(key, "DOCUMENTS__") {
		t.Fatalf("binding key %q is not scoped by its module", key)
	}
	var prepared PreparedRunnable
	if err := json.Unmarshal([]byte(value), &prepared); err != nil {
		t.Fatal(err)
	}
	pkg := &basev0.RunnablePackage{}
	binding := &basev0.RunnableBinding{}
	if err := protojson.Unmarshal(prepared.Package, pkg); err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(prepared.Binding, binding); err != nil {
		t.Fatal(err)
	}
	if err := corerunnable.VerifyBinding(binding, pkg); err != nil {
		t.Fatal("binding does not verify against its package: ", err)
	}
	endpoint := binding.GetTarget().GetService().GetEndpoint()
	if endpoint.GetEndpoint().GetName() != "grpc" || len(endpoint.GetInstances()) != 1 || !strings.Contains(endpoint.GetInstances()[0].GetAddress(), "localhost:") {
		t.Fatalf("binding does not target the owner's resolved local address: %v", endpoint)
	}
	if binding.GetTarget().GetEnvironment() != "local" || !strings.HasPrefix(binding.GetTarget().GetRevision(), "sha256:") {
		t.Fatalf("binding target identity: %v", binding.GetTarget())
	}
	if prepared.Operation == nil || prepared.Operation.Method != pkg.GetServiceOperations()[0].GetOperation() {
		t.Fatalf("operation policy not carried: %+v", prepared.Operation)
	}
	descriptors, err := base64.StdEncoding.DecodeString(prepared.Descriptors)
	if err != nil {
		t.Fatal(err)
	}
	set := &descriptorpb.FileDescriptorSet{}
	if err := googleproto.Unmarshal(descriptors, set); err != nil || len(set.GetFile()) == 0 {
		t.Fatal("owner descriptors missing: ", err)
	}

	// The committed file is current; a stale one is refused by --check.
	runnableBindingsCheck = true
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(raw, []byte("STALE=1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil); err == nil {
		t.Fatal("--check accepted a stale file")
	}
}

func TestRunnableBindingKeyIsAnEnvironmentKey(t *testing.T) {
	if got := RunnableBindingKey("documents", "documents.ingest-document"); got != "DOCUMENTS__DOCUMENTS_INGEST_DOCUMENT" {
		t.Fatalf("key %q", got)
	}
}

// A prepared binding carries the owner's descriptors so a generic caller can
// resolve the method; it is delivered as configuration, which a run carries in
// the process environment, so source locations and comments — most of a
// descriptor's bytes and nothing a caller resolves with — are dropped, while the
// closure and the method stay resolvable.
func TestDescriptorClosureCarriesNoSourceInfo(t *testing.T) {
	owner := &descriptorpb.FileDescriptorProto{
		Name: googleproto.String("owner/v1/api.proto"), Package: googleproto.String("owner.v1"),
		Dependency:  []string{"owner/v1/types.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{Name: googleproto.String("Req")}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: googleproto.String("Owner"), Method: []*descriptorpb.MethodDescriptorProto{{
			Name: googleproto.String("Apply"), InputType: googleproto.String(".owner.v1.Req"), OutputType: googleproto.String(".owner.v1.Res"),
		}}}},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{Path: []int32{4, 0}, Span: []int32{1, 0, 3}, LeadingComments: googleproto.String(" a long comment")}}},
		Syntax:         googleproto.String("proto3"),
	}
	types := &descriptorpb.FileDescriptorProto{
		Name: googleproto.String("owner/v1/types.proto"), Package: googleproto.String("owner.v1"),
		MessageType:    []*descriptorpb.DescriptorProto{{Name: googleproto.String("Res")}},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{Path: []int32{4, 0}, Span: []int32{1, 0, 3}}}},
		Syntax:         googleproto.String("proto3"),
	}
	unrelated := &descriptorpb.FileDescriptorProto{Name: googleproto.String("other/v1/x.proto"), Package: googleproto.String("other.v1"), Syntax: googleproto.String("proto3")}
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{types, owner, unrelated}}

	out := descriptorClosure(set, "/owner.v1.Owner/Apply")
	if len(out.GetFile()) != 2 {
		t.Fatalf("closure holds %d files, want the method's file and its import", len(out.GetFile()))
	}
	for _, file := range out.GetFile() {
		if file.GetSourceCodeInfo() != nil {
			t.Fatalf("%s carries source info", file.GetName())
		}
	}
	if owner.GetSourceCodeInfo() == nil {
		t.Fatal("the contract's own descriptors were modified")
	}
	files, err := protodesc.NewFiles(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.FindDescriptorByName("owner.v1.Owner.Apply"); err != nil {
		t.Fatal("the method no longer resolves: ", err)
	}
}
