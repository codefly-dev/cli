package composition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceInspectionNeverAcquiresOrChoosesFloatingReleases(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	t.Setenv(ModuleCacheEnv, cache)
	bin := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(bin, 0755))
	log := filepath.Join(root, "git-invoked")
	t.Setenv("INSPECTION_GIT_LOG", log)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho invoked >> \"$INSPECTION_GIT_LOG\"\nexit 1\n"), 0700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := WithWorkspaceInspection(context.Background())
	ref := &resources.WorkspaceReference{Name: "base", Source: "example/base", Version: "1.2.3", Workspace: "composition"}
	_, err := ResolveWorkspaceReference(ctx, ref)
	require.ErrorContains(t, err, "not cached")
	_, err = os.Stat(cache)
	require.True(t, os.IsNotExist(err), "inspection must not create the cache")
	dir := filepath.Join(cache, "example/base/v1.2.3/composition")
	require.NoError(t, os.MkdirAll(dir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: base\nlayout: modules\n"), 0600))
	got, err := ResolveWorkspaceReference(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, dir, got)
	for _, version := range []string{"latest", ">=1.0.0", "1.x"} {
		ref.Version = version
		_, err := ResolveWorkspaceReference(ctx, ref)
		require.ErrorContains(t, err, "exact workspace release", "cached tags must not turn a floating request into an apparent selection")
	}
	_, err = os.Stat(log)
	require.True(t, os.IsNotExist(err), "inspection must not invoke git")
}
