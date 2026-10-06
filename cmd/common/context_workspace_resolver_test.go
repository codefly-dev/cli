package common_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A `workspaces:` entry naming a release has no directory until a host produces
// one. Core delegates that to a resolver on the context and refuses with
// "requires host resolution" when none is registered.
//
// The root command registers one on `cmd.Context()`. That is not enough: 121 of
// this CLI's RunE bodies discard the *cobra.Command (`func(_ *cobra.Command,
// …)`) and build their context with common.NewContext instead, so the root's
// registration never reached them. `codefly doctor workspace` reads
// cmd.Context() and resolved a released import; `codefly deploy gitops render`
// does not, and died with
//
//	workspace "platform-core" at obin-ai/platform-core@0.0.2 requires host resolution
//
// on a workspace the same binary had just loaded successfully.
//
// Offline on purpose. An explicit semver version resolves to its tag without
// asking the remote, and a populated checkout in the cache skips the clone, so
// pre-seeding the cache exercises the whole path — resolver reached, directory
// returned, imported workspace composed — with no network.
func TestNewContextCarriesTheWorkspaceResolver(t *testing.T) {
	cache := t.TempDir()
	t.Setenv(composition.ModuleCacheEnv, cache)

	imported := filepath.Join(cache, "team", "platform-core", "v1.0.0")
	require.NoError(t, os.MkdirAll(imported, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(imported, "workspace.codefly.yaml"),
		[]byte("name: platform-core\nlayout: modules\n"), 0o600))

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"),
		[]byte("name: product\nlayout: modules\nworkspaces:\n  - name: platform-core\n    source: team/platform-core\n    version: 1.0.0\n"), 0o600))

	ctx, done := common.NewContext()
	defer done()

	ws, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err, "common.NewContext must register the workspace resolver: every command that discards *cobra.Command starts from it")
	require.Equal(t, "product", ws.Name)
}
