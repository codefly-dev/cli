package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// A service that keeps the scaffold's contract and depends on a client
// generated from another scaffolded contract is refused before it starts,
// with what collides and the command that fixes it.
func TestProtoNameCollisionErrorNamesTheFix(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write := func(content string, parts ...string) {
		t.Helper()
		path := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("name: test\nlayout: modules\nmodules:\n    - name: phone\n", resources.WorkspaceConfigurationName)
	write("kind: module\nname: phone\nservices:\n    - name: calls\n    - name: plain\n", "modules", "phone", "module.codefly.yaml")
	const agent = "version: 0.0.0\nagent:\n  kind: codefly:service\n  name: fixture\n  publisher: codefly.dev\n  version: 1.0.0\n"
	write("name: calls\n"+agent+"library-dependencies:\n    - name: shop-client\n      languages:\n        - go\n", "modules", "phone", "services", "calls", "service.codefly.yaml")
	write("syntax = \"proto3\";\npackage api;\nmessage VersionRequest {}\n", "modules", "phone", "services", "calls", "proto", "api.proto")
	write("name: plain\n"+agent, "modules", "phone", "services", "plain", "service.codefly.yaml")
	write("kind: library\nname: shop-client\nversion: 0.0.0\n", "libraries", "shop-client", resources.LibraryConfigurationName)
	contract, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("api.proto"), Package: proto.String("api"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("VersionRequest")}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	write(string(contract), "libraries", "shop-client", "contract", "contract.binpb")

	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	module, err := workspace.LoadModuleFromName(ctx, "phone")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := module.LoadServiceFromName(ctx, "calls")
	if err != nil {
		t.Fatal(err)
	}
	err = ProtoNameCollisionError(ctx, workspace, calls, "phone/calls")
	if err == nil {
		t.Fatal("a service loading api.proto twice must be refused")
	}
	for _, want := range []string{"phone/calls would load the same protobuf names twice", `proto file "api.proto"`, "library shop-client", "codefly fix proto-names"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error misses %q: %v", want, err)
		}
	}
	plain, err := module.LoadServiceFromName(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if err := ProtoNameCollisionError(ctx, workspace, plain, "phone/plain"); err != nil {
		t.Fatalf("a service with no client library loads one set of names: %v", err)
	}
}
