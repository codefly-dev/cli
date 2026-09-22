package generate

import (
	"os"
	"path/filepath"
	"reflect"
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
	if _, err := protoMountRoot("/repo/proto", "/repo/output", "/elsewhere/templates"); err == nil {
		t.Fatal("accepted writable mount of the filesystem root")
	}
	root, err := protoMountRoot("/repo/proto", "/repo/service/output", "/repo/templates")
	if err != nil || root != "/repo" {
		t.Fatalf("mount root = %q, %v", root, err)
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
