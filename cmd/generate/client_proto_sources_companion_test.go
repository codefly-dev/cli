//go:build proto_companion_required

package generate

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// sourceNames is the source set buf names for a proto tree, through the
// pinned companion.
func sourceNames(t *testing.T, dir string) []string {
	t.Helper()
	_, sources, err := compileProtoDir(t.Context(), dir, true)
	require.NoError(t, err)
	var names []string
	for _, source := range sources {
		names = append(names, source.Path)
	}
	sort.Strings(names)
	return names
}

func writeProtos(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		writeTestFile(t, filepath.Join(root, rel), "syntax = \"proto3\";\n")
	}
}

// A v2 buf.yaml with a vendor module and an excluded tree: every source is
// named relative to the module that holds it, and an excluded tree is not a
// source.
func TestBufNamesV2ModulesAndExcludesThroughPinnedCompanion(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n    excludes: [facade, vendor]\n  - path: vendor\n")
	writeProtos(t, dir, "acme/v1/api.proto", "facade/api.proto", "vendor/upstream/base/v0/base.proto")
	require.Equal(t, []string{"acme/v1/api.proto", "upstream/base/v0/base.proto"}, sourceNames(t, dir))
}

// A v1 buf.yaml's build.excludes keep a tree out of the source set.
func TestBufNamesV1BuildExcludesThroughPinnedCompanion(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "buf.yaml"), "version: v1\nbuild:\n  excludes:\n    - facade\n")
	writeProtos(t, dir, "acme/v1/api.proto", "facade/api.proto")
	require.Equal(t, []string{"acme/v1/api.proto"}, sourceNames(t, dir))
}

// A v2 module's includes narrow it to the trees named.
func TestBufNamesV2IncludesThroughPinnedCompanion(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n    includes: [acme]\n")
	writeProtos(t, dir, "acme/v1/api.proto", "other/v1/other.proto")
	require.Equal(t, []string{"acme/v1/api.proto"}, sourceNames(t, dir))
}

// A module folder that is a symlink contributes its files under their import
// names: the link's target is copied in, since it is not mounted.
func TestBufNamesASymlinkedModuleThroughPinnedCompanion(t *testing.T) {
	root := t.TempDir()
	dir, shared := filepath.Join(root, "proto"), filepath.Join(root, "shared")
	writeProtos(t, shared, "upstream/base/v0/base.proto")
	writeTestFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: .\n    excludes: [vendor]\n  - path: vendor\n")
	writeProtos(t, dir, "acme/v1/api.proto")
	require.NoError(t, os.Symlink(shared, filepath.Join(dir, "vendor")))
	require.Equal(t, []string{"acme/v1/api.proto", "upstream/base/v0/base.proto"}, sourceNames(t, dir))
}
