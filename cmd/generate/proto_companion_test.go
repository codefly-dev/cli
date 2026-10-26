//go:build proto_companion_required

package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtoCustomTemplateAndPathThroughPinnedCompanion(t *testing.T) {
	root := t.TempDir()
	input, output := filepath.Join(root, "proto"), filepath.Join(root, "service")
	writeTestFile(t, filepath.Join(input, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n")
	for _, name := range []string{"selected", "excluded"} {
		writeTestFile(t, filepath.Join(input, name+".proto"), `syntax = "proto3";
package example.v1;
option go_package = "example.com/generated;example";
message `+name+` { string value = 1; }
`)
	}
	writeTestFile(t, filepath.Join(output, "buf.custom.yaml"), `version: v2
plugins:
  - local: protoc-gen-go
    out: generated
    opt: paths=source_relative
`)
	oldTemplate, oldLocal, oldPaths := protoTemplate, protoLocal, protoPaths
	t.Cleanup(func() { protoTemplate, protoLocal, protoPaths = oldTemplate, oldLocal, oldPaths })
	protoTemplate, protoLocal, protoPaths = "buf.custom.yaml", true, []string{"selected.proto"}
	require.NoError(t, generateProtoCode(context.Background(), input, output))
	generated := filepath.Join(output, "generated", "selected.pb.go")
	first, err := os.ReadFile(generated)
	require.NoError(t, err)
	require.Contains(t, string(first), "type Selected struct")
	_, err = os.Stat(filepath.Join(output, "generated", "excluded.pb.go"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, generateProtoCode(context.Background(), input, output))
	second, err := os.ReadFile(generated)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

// The go-grpc service layout: buf.gen.yaml in proto/, --output the proto
// directory, and outputs in sibling directories. Before #836 buf wrote them
// inside the container and the run reported success with nothing on disk.
func TestProtoSiblingOutputsThroughPinnedCompanion(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), "name: example\n")
	service := filepath.Join(root, "svc")
	input := filepath.Join(service, "proto")
	writeTestFile(t, filepath.Join(input, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n")
	writeTestFile(t, filepath.Join(input, "api.proto"), `syntax = "proto3";
package example.v1;
option go_package = "example.com/svc/pkg/gen;gen";
message Item { string value = 1; }
`)
	writeTestFile(t, filepath.Join(input, "buf.gen.yaml"), `version: v2
plugins:
  - local: protoc-gen-go
    out: ../code/pkg/gen
    opt: paths=source_relative
`)
	oldTemplate, oldLocal, oldPaths := protoTemplate, protoLocal, protoPaths
	t.Cleanup(func() { protoTemplate, protoLocal, protoPaths = oldTemplate, oldLocal, oldPaths })
	protoTemplate, protoLocal, protoPaths = "", false, []string{"api.proto"}
	require.NoError(t, generateProtoCode(context.Background(), input, input))
	generated := filepath.Join(service, "code", "pkg", "gen", "api.pb.go")
	first, err := os.ReadFile(generated)
	require.NoError(t, err)
	require.Contains(t, string(first), "type Item struct")
	// A no-change regeneration rewrites the same bytes and still succeeds.
	require.NoError(t, generateProtoCode(context.Background(), input, input))
	second, err := os.ReadFile(generated)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestProtoTemplateInputsAndPathsThroughPinnedCompanion(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), "name: example\n")
	input := filepath.Join(root, "proto")
	for _, dir := range []string{"proto", "second"} {
		writeTestFile(t, filepath.Join(root, dir, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n")
	}
	for _, name := range []string{"proto/selected", "proto/excluded", "second/extra"} {
		writeTestFile(t, filepath.Join(root, name+".proto"), `syntax = "proto3";
package example.v1;
option go_package = "example.com/generated;example";
message `+filepath.Base(name)+` { string value = 1; }
`)
	}
	writeTestFile(t, filepath.Join(input, "buf.gen.yaml"), `version: v2
inputs:
  - directory: .
    paths: [selected.proto]
  - directory: ../second
plugins:
  - local: protoc-gen-go
    out: ../code/gen
    opt: paths=source_relative
`)
	oldTemplate, oldLocal, oldPaths := protoTemplate, protoLocal, protoPaths
	t.Cleanup(func() { protoTemplate, protoLocal, protoPaths = oldTemplate, oldLocal, oldPaths })
	protoTemplate, protoLocal, protoPaths = "", false, nil
	require.NoError(t, generateProtoCode(t.Context(), input, input))
	_, err := os.Stat(filepath.Join(root, "code/gen/excluded.pb.go"))
	require.ErrorIs(t, err, os.ErrNotExist, "a positional input must not replace the template's path selection")
	for _, name := range []string{"selected", "extra"} {
		contents, err := os.ReadFile(filepath.Join(root, "code/gen", name+".pb.go"))
		require.NoError(t, err, "every template input must be generated")
		require.Contains(t, string(contents), "package example")
	}
	// An explicit caller selection retains Buf's override semantics, rather
	// than intersecting with the template's selected.proto list.
	writeTestFile(t, filepath.Join(input, "buf.gen.yaml"), `version: v2
clean: true
inputs:
  - directory: .
    paths: [selected.proto]
plugins:
  - local: protoc-gen-go
    out: ../code/gen
    opt: paths=source_relative
`)
	protoPaths = []string{"excluded.proto"}
	require.NoError(t, generateProtoCode(t.Context(), input, input))
	_, err = os.Stat(filepath.Join(root, "code/gen/excluded.pb.go"))
	require.NoError(t, err)
	for _, name := range []string{"selected", "extra"} {
		_, err := os.Stat(filepath.Join(root, "code/gen", name+".pb.go"))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

// The document store's layout: a template declaring two inputs (its own protos
// with a path selection, and a sibling tree of client contracts), generated
// with a caller --path. buf applies --path to every input, so keeping both
// inputs failed with "no .proto files were targeted"; the caller's selection
// makes --proto the one input.
func TestProtoTwoInputTemplateWithCallerPathThroughPinnedCompanion(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "workspace.codefly.yaml"), "name: example\n")
	input := filepath.Join(root, "proto")
	for _, dir := range []string{"proto", "deps"} {
		writeTestFile(t, filepath.Join(root, dir, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n")
	}
	for _, name := range []string{"proto/api/v1/api", "proto/api/v1/other", "deps/client/v1/client"} {
		writeTestFile(t, filepath.Join(root, name+".proto"), `syntax = "proto3";
package example.v1;
option go_package = "example.com/generated;example";
message `+filepath.Base(name)+` { string value = 1; }
`)
	}
	writeTestFile(t, filepath.Join(input, "buf.gen.yaml"), `version: v2
inputs:
  - directory: .
    paths: [api/v1/api.proto]
  - directory: ../deps
plugins:
  - local: protoc-gen-go
    out: ../code/gen
    opt: paths=source_relative
`)
	oldTemplate, oldLocal, oldPaths := protoTemplate, protoLocal, protoPaths
	t.Cleanup(func() { protoTemplate, protoLocal, protoPaths = oldTemplate, oldLocal, oldPaths })
	protoTemplate, protoLocal, protoPaths = "", false, []string{"api/v1/api.proto"}
	require.NoError(t, generateProtoCode(t.Context(), input, input))
	_, err := os.Stat(filepath.Join(root, "code/gen/api/v1/api.pb.go"))
	require.NoError(t, err, "the caller's file is generated")
	for _, name := range []string{"api/v1/other.pb.go", "client/v1/client.pb.go"} {
		_, err := os.Stat(filepath.Join(root, "code/gen", name))
		require.ErrorIs(t, err, os.ErrNotExist, "only the caller's selection is generated: %s", name)
	}
}
