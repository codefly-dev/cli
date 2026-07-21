package generate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGeneratedGoOutputDirsUsesTemplateTopology(t *testing.T) {
	outputDir := t.TempDir()
	writeTestFile(t, filepath.Join(outputDir, "buf.gen.local.yaml"), `version: v2
plugins:
  - local: [go, run, example/go]
    out: code/pkg/gen
  - local: [go, run, example/go-grpc]
    out: code/pkg/gen
  - local: [npx, protoc-gen-es]
    out: ../frontend/src/gen
  - local: [go, run, example/connect]
    out: generated/connect
`)
	writeTestFile(t, filepath.Join(outputDir, "code/pkg/gen/service.pb.go"), "package gen\n")
	writeTestFile(t, filepath.Join(outputDir, "generated/connect/service.connect.go"), "package connect\n")
	writeTestFile(t, filepath.Join(outputDir, "../frontend/src/gen/service_pb.ts"), "export {};\n")

	got, err := generatedGoOutputDirs(outputDir, "buf.gen.local.yaml")
	if err != nil {
		t.Fatalf("generatedGoOutputDirs: %v", err)
	}
	want := []string{
		filepath.Join(outputDir, "code/pkg/gen"),
		filepath.Join(outputDir, "generated/connect"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("generatedGoOutputDirs() = %v, want %v", got, want)
	}
}

func TestGeneratedGoOutputDirsSkipsMissingAndNonGoOutputs(t *testing.T) {
	outputDir := t.TempDir()
	writeTestFile(t, filepath.Join(outputDir, "buf.gen.local.yaml"), `version: v2
plugins:
  - plugin: buf.build/community/neoeinstein-prost
    out: missing
  - plugin: buf.build/bufbuild/es
    out: typescript
`)
	writeTestFile(t, filepath.Join(outputDir, "typescript/service_pb.ts"), "export {};\n")

	got, err := generatedGoOutputDirs(outputDir, "buf.gen.local.yaml")
	if err != nil {
		t.Fatalf("generatedGoOutputDirs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("generatedGoOutputDirs() = %v, want no Go outputs", got)
	}
}

func TestGeneratedGoOutputDirsRejectsMalformedTemplate(t *testing.T) {
	outputDir := t.TempDir()
	writeTestFile(t, filepath.Join(outputDir, "buf.gen.local.yaml"), "plugins: [\n")

	if _, err := generatedGoOutputDirs(outputDir, "buf.gen.local.yaml"); err == nil {
		t.Fatal("generatedGoOutputDirs() succeeded for malformed YAML")
	}
}

func TestGeneratedGoImportsInvocationUsesGeneratedRootAsWorkingDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "service", "code", "pkg", "gen")

	workingDir, args := generatedGoImportsInvocation(root)
	if workingDir != root {
		t.Fatalf("working directory = %q, want generated root %q", workingDir, root)
	}
	wantArgs := []string{"-w", "."}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("arguments = %v, want %v", args, wantArgs)
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
