package generate

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestProtoGenerationPathArgs(t *testing.T) {
	previous := protoPaths
	t.Cleanup(func() { protoPaths = previous })
	protoPaths = []string{"mind/gateway/v1/gateway.proto", "mind/v1/mind.proto"}

	if got, want := protoGenerationPathArgs("/workspace/proto", false), []string{
		"--path", "mind/gateway/v1/gateway.proto", "--path", "mind/v1/mind.proto",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relative path args = %v, want %v", got, want)
	}
	if got, want := protoGenerationPathArgs("/workspace/proto", true), []string{
		"--path", "/workspace/proto/mind/gateway/v1/gateway.proto",
		"--path", "/workspace/proto/mind/v1/mind.proto",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("absolute path args = %v, want %v", got, want)
	}
}

func TestResolveProtoTemplate(t *testing.T) {
	root := t.TempDir()
	input, output := filepath.Join(root, "proto"), filepath.Join(root, "generated")
	for _, path := range []string{
		filepath.Join(input, "buf.gen.yaml"),
		filepath.Join(output, "buf.gen.local.yaml"),
		filepath.Join(output, "sdk", "buf.gen.yaml"),
	} {
		writeTestFile(t, path, "version: v2\nplugins: []\n")
	}
	for _, tc := range []struct {
		name, template, want string
		local                bool
	}{
		{"default", "", filepath.Join(input, "buf.gen.yaml"), false},
		{"local", "", filepath.Join(output, "buf.gen.local.yaml"), true},
		{"relative", "sdk/buf.gen.yaml", filepath.Join(output, "sdk", "buf.gen.yaml"), false},
		{"explicit overrides local", "sdk/buf.gen.yaml", filepath.Join(output, "sdk", "buf.gen.yaml"), true},
		{"absolute", filepath.Join(input, "buf.gen.yaml"), filepath.Join(input, "buf.gen.yaml"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveProtoTemplate(input, output, tc.template, tc.local)
			if err != nil || got != tc.want {
				t.Fatalf("template = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"missing.yaml", "sdk"} {
		if _, err := resolveProtoTemplate(input, output, bad, false); err == nil {
			t.Fatalf("accepted invalid template %q", bad)
		}
	}
}

func TestProtoMountRootNeverExposesFilesystemRoot(t *testing.T) {
	if _, err := protoMountRoot("/repo/proto", "/repo/output", "/elsewhere/templates", nil, ""); err == nil {
		t.Fatal("accepted writable mount of the filesystem root")
	}
	root, err := protoMountRoot("/repo/proto", "/repo/service/output", "/repo/templates", nil, "")
	if err != nil || root != "/repo" {
		t.Fatalf("mount root = %q, %v", root, err)
	}
}

// The go-grpc service layout keeps buf.gen.yaml in proto/ with its outputs in
// sibling directories. The mount must reach every one of them, or buf writes
// them inside the container and they are lost (#836).
func TestProtoMountRootCoversSiblingOutputs(t *testing.T) {
	workspace := t.TempDir()
	service := filepath.Join(workspace, "mod", "svc")
	input := filepath.Join(service, "proto")
	template := filepath.Join(input, "buf.gen.yaml")
	writeTestFile(t, template, `version: v2
plugins:
  - local: protoc-gen-go
    out: ../code/pkg/gen
    opt: paths=source_relative
  - local: protoc-gen-go-grpc
    out: ../code/pkg/gen
  - local: protoc-gen-openapiv2
    out: ../openapi
`)
	outs, err := protoTemplateOutputs(template)
	if err != nil {
		t.Fatalf("template outputs: %v", err)
	}
	if want := []string{filepath.Join(service, "code", "pkg", "gen"), filepath.Join(service, "openapi")}; !reflect.DeepEqual(outs, want) {
		t.Fatalf("outputs = %v, want %v", outs, want)
	}
	root, err := protoMountRoot(input, input, input, outs, workspace)
	if err != nil {
		t.Fatalf("mount root: %v", err)
	}
	if root != service {
		t.Fatalf("mount root = %q, want %q", root, service)
	}
	for _, path := range append([]string{input}, outs...) {
		if !pathWithin(root, path) {
			t.Fatalf("mount root %s does not cover %s", root, path)
		}
	}
}

func TestProtoMountRootRefusesOutputEscapingBoundary(t *testing.T) {
	workspace := t.TempDir()
	input := filepath.Join(workspace, "svc", "proto")
	template := filepath.Join(input, "buf.gen.yaml")
	writeTestFile(t, template, "version: v2\nplugins:\n  - local: protoc-gen-go\n    out: ../../../elsewhere\n")
	outs, err := protoTemplateOutputs(template)
	if err != nil {
		t.Fatalf("template outputs: %v", err)
	}
	if _, err := protoMountRoot(input, input, input, outs, workspace); err == nil {
		t.Fatal("accepted an output outside the workspace")
	}
	// Outside a workspace the boundary is the directories the caller named.
	if _, err := protoMountRoot(input, input, input, outs, ""); err == nil {
		t.Fatal("accepted an output outside the named directories")
	}
	writeTestFile(t, template, "version: v2\nplugins:\n  - local: protoc-gen-go\n    out: /abs/gen\n")
	if _, err := protoTemplateOutputs(template); err == nil {
		t.Fatal("accepted an absolute output the companion cannot see")
	}
}

func TestProtoMountBoundaryIsTheOwningWorkspace(t *testing.T) {
	workspace := t.TempDir()
	writeTestFile(t, filepath.Join(workspace, "workspace.codefly.yaml"), "name: example\n")
	input := filepath.Join(workspace, "mod", "svc", "proto")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := protoMountBoundary(context.Background(), input)
	if err != nil || got != workspace {
		t.Fatalf("boundary = %q, %v; want %q", got, err, workspace)
	}
}

// A template that lists outputs but leaves every one untouched must fail: that
// is exactly what buf writing into the container looks like from the host.
func TestRequireProtoOutputsWrittenRefusesSilentNoOp(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "gen")
	committed := filepath.Join(out, "api.pb.go")
	writeTestFile(t, committed, "package gen\n")
	outs := []string{out, filepath.Join(root, "openapi")}

	before, err := snapshotProtoOutputs(outs)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := requireProtoOutputsWritten(outs, before, "buf.gen.yaml"); err == nil {
		t.Fatal("reported success for a generation that wrote nothing")
	}

	// A rewrite of identical bytes is a real regeneration.
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(committed, later, later); err != nil {
		t.Fatal(err)
	}
	if err := requireProtoOutputsWritten(outs, before, "buf.gen.yaml"); err != nil {
		t.Fatalf("refused a rewritten output: %v", err)
	}

	// So is a file in an output that did not exist before.
	empty, err := snapshotProtoOutputs(outs[1:])
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(outs[1], "api.swagger.json"), "{}")
	if err := requireProtoOutputsWritten(outs[1:], empty, "buf.gen.yaml"); err != nil {
		t.Fatalf("refused a new output: %v", err)
	}

	if err := requireProtoOutputsWritten(nil, nil, "buf.gen.yaml"); err != nil {
		t.Fatalf("a template without outputs cannot be judged: %v", err)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
