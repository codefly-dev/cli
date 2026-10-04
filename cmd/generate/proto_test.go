package generate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

// The contract the output validation rests on: buf syncs an output tree
// rather than rewriting it, so a correct unchanged replay touches nothing
// (#885). The gate is therefore the mount, checked before generation — the
// companion must be looking at the host's own output directory.
//
// faithfulMount stands in for a companion whose `out` really is the host
// directory, by translating the container path back and removing the probe.
func faithfulMount(hostRoot, containerRoot string) protoCommand {
	return func(_ context.Context, bin string, args ...string) error {
		if bin != "rm" {
			return fmt.Errorf("unexpected command %q", bin)
		}
		for _, arg := range args {
			if arg == "--" {
				continue
			}
			rel, err := filepath.Rel(containerRoot, arg)
			if err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(hostRoot, rel)); err != nil {
				return err
			}
		}
		return nil
	}
}

func protoOutputFixture(t *testing.T) (root string, outs []string) {
	t.Helper()
	root = t.TempDir()
	outs = []string{filepath.Join(root, "sdk", "src", "gen"), filepath.Join(root, "svc", "openapi")}
	return root, outs
}

func TestVerifyProtoOutputMountsAcceptsTheHostsOwnOutputs(t *testing.T) {
	root, outs := protoOutputFixture(t)
	// An unchanged replay: the tree is already the generated one.
	committed := filepath.Join(outs[0], "api_pb.ts")
	writeTestFile(t, committed, "export {};\n")
	before, err := os.Stat(committed)
	if err != nil {
		t.Fatal(err)
	}

	probe := protoOutputProbe("proto-gen-1")
	if err := verifyProtoOutputMounts(context.Background(), faithfulMount(root, "/workspace"), outs, root, "/workspace", probe); err != nil {
		t.Fatalf("refused a live mount: %v", err)
	}

	// The check leaves the tree exactly as it found it, probe included: a
	// stray file under a generated output is what a consumer's drift gate
	// reports, and an unchanged replay must stay unchanged.
	for _, out := range outs {
		if _, err := os.Lstat(filepath.Join(out, probe)); !os.IsNotExist(err) {
			t.Fatalf("probe left behind in %s: %v", out, err)
		}
	}
	after, err := os.Stat(committed)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("the mount check rewrote a generated file")
	}
}

// A first generation has no output directory yet. The host creates it, so the
// tree a consumer commits is host-owned rather than owned by whatever user the
// companion runs as.
func TestVerifyProtoOutputMountsCreatesAMissingOutput(t *testing.T) {
	root, outs := protoOutputFixture(t)
	if err := verifyProtoOutputMounts(context.Background(), faithfulMount(root, "/workspace"), outs, root, "/workspace", protoOutputProbe("proto-gen-2")); err != nil {
		t.Fatalf("refused a first generation: %v", err)
	}
	for _, out := range outs {
		info, err := os.Stat(out)
		if err != nil || !info.IsDir() {
			t.Fatalf("output %s was not created: %v", out, err)
		}
	}
}

// The failure the old mtime guard was a proxy for (#836): buf resolves `out`
// inside the container, writes there, and the host keeps the tree it already
// had. The companion cannot delete a probe it cannot see.
func TestVerifyProtoOutputMountsRefusesAContainerLocalOutput(t *testing.T) {
	root, outs := protoOutputFixture(t)
	writeTestFile(t, filepath.Join(outs[0], "api_pb.ts"), "export {};\n")
	probe := protoOutputProbe("proto-gen-3")

	blind := func(_ context.Context, _ string, _ ...string) error {
		return fmt.Errorf("rm: can't remove '/workspace/sdk/src/gen/%s': No such file or directory", probe)
	}
	err := verifyProtoOutputMounts(context.Background(), blind, outs, root, "/workspace", probe)
	if err == nil {
		t.Fatal("accepted an output the companion cannot reach")
	}
	if !strings.Contains(err.Error(), outs[0]) {
		t.Fatalf("error does not name the unreachable output: %v", err)
	}
	for _, out := range outs {
		if _, err := os.Lstat(filepath.Join(out, probe)); !os.IsNotExist(err) {
			t.Fatalf("probe left behind in %s after a failed check: %v", out, err)
		}
	}
}

// A mount the companion can read but whose writes never reach the host: the
// command exits clean and the probe survives. Exit status alone would pass
// this, which is why the host checks the probe is gone.
func TestVerifyProtoOutputMountsRefusesAMountThatDiscardsWrites(t *testing.T) {
	root, outs := protoOutputFixture(t)
	probe := protoOutputProbe("proto-gen-4")
	discarding := func(_ context.Context, _ string, _ ...string) error { return nil }

	err := verifyProtoOutputMounts(context.Background(), discarding, outs, root, "/workspace", probe)
	if err == nil {
		t.Fatal("accepted a mount that discards what the companion writes")
	}
	if !strings.Contains(err.Error(), outs[0]) {
		t.Fatalf("error does not name the output: %v", err)
	}
	for _, out := range outs {
		if _, err := os.Lstat(filepath.Join(out, probe)); !os.IsNotExist(err) {
			t.Fatalf("probe left behind in %s: %v", out, err)
		}
	}
}

func TestVerifyProtoOutputMountsRefusesAnOutputOutsideTheMount(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "gen")
	called := false
	companion := func(_ context.Context, _ string, _ ...string) error {
		called = true
		return nil
	}
	err := verifyProtoOutputMounts(context.Background(), companion, []string{outside}, root, "/workspace", protoOutputProbe("proto-gen-5"))
	if err == nil {
		t.Fatal("accepted an output outside the companion mount")
	}
	if called {
		t.Fatal("ran the companion for an output it could never reach")
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatal("probed an output outside the mount")
	}
}

// A template declaring no output cannot be judged either way.
func TestVerifyProtoOutputMountsSkipsATemplateWithoutOutputs(t *testing.T) {
	called := false
	companion := func(_ context.Context, _ string, _ ...string) error {
		called = true
		return nil
	}
	if err := verifyProtoOutputMounts(context.Background(), companion, nil, t.TempDir(), "/workspace", "probe"); err != nil {
		t.Fatalf("refused a template without outputs: %v", err)
	}
	if called {
		t.Fatal("ran the companion with nothing to check")
	}
}

func TestRequireProtoOutputsPopulated(t *testing.T) {
	_, outs := protoOutputFixture(t)

	// No generation at all: every declared output is absent or empty.
	if err := requireProtoOutputsPopulated(outs, "buf.gen.yaml"); err == nil {
		t.Fatal("reported success for a generation that produced nothing")
	}
	for _, out := range outs {
		if err := os.MkdirAll(filepath.Join(out, "documents", "v1"), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := requireProtoOutputsPopulated(outs, "buf.gen.yaml"); err == nil {
		t.Fatal("a tree of empty directories is not generated output")
	}

	// The #885 regression: an unchanged replay writes nothing, and the tree it
	// left alone is the generated tree.
	writeTestFile(t, filepath.Join(outs[0], "documents", "v1", "api_pb.ts"), "export {};\n")
	if err := requireProtoOutputsPopulated(outs, "buf.gen.yaml"); err != nil {
		t.Fatalf("refused an unchanged replay: %v", err)
	}

	// Any output, not every output: a plugin with nothing to emit for this
	// input (openapiv2 over a contract with no REST annotations) is not a
	// failed generation.
	if err := requireProtoOutputsPopulated(outs[1:], "buf.gen.yaml"); err == nil {
		t.Fatal("reported success with every declared output empty")
	}
	if err := requireProtoOutputsPopulated(nil, "buf.gen.yaml"); err != nil {
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
