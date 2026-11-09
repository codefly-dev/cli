package methodpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
)

// A method's declaration is read with what a call may carry beyond what it
// requires (authority.hop.v1.Policy.carries), beside requires.
func TestAMethodDeclarationIsReadWithWhatItCarries(t *testing.T) {
	options := &descriptorpb.MethodOptions{}
	proto.SetExtension(options, hopv1.E_Policy, &hopv1.Policy{
		Requires: []*hopv1.Requirement{{ResourceKind: "documents", Actions: []string{"write"}}},
		Carries:  []*hopv1.Requirement{{ResourceKind: "documents.collection", Actions: []string{"create"}}},
	})
	set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("documents/v1/api.proto"), Package: proto.String("documents.v1"),
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("DocumentsService"), Method: []*descriptorpb.MethodDescriptorProto{{
			Name: proto.String("CreateDocument"), InputType: proto.String(".google.protobuf.Empty"), OutputType: proto.String(".google.protobuf.Empty"), Options: options,
		}}}},
	}}}
	data, err := proto.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "contract.binpb")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	methods, _, err := contractMethods(path, "sha256:"+hex.EncodeToString(sum[:]), func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || len(methods[0].GetRequires()) != 1 || len(methods[0].GetCarries()) != 1 ||
		methods[0].GetCarries()[0].GetResourceKind() != "documents.collection" || methods[0].GetCarries()[0].GetActions()[0] != "create" {
		t.Fatalf("read %v", methods)
	}
}
