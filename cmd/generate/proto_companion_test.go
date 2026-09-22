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
