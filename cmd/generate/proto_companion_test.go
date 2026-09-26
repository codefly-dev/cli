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
