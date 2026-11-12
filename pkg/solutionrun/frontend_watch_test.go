package solutionrun

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/builders"
	"github.com/stretchr/testify/require"
)

func TestFrontendStagingRejectsChangedSnapshot(t *testing.T) {
	_, module := wikiRepository(t)
	sources, err := selectFrontendSources(t.Context(), module)
	require.NoError(t, err)
	inputs, _, err := sources.digest(module, false)
	require.NoError(t, err)
	staging := t.TempDir()
	require.NoError(t, sources.stage(t.Context(), staging, module, false, inputs))
	file := filepath.Join(module.ServicesDir(), "fe-remote", "Page.tsx")
	require.NoError(t, os.WriteFile(file, []byte("changed after discovery"), 0600))
	require.ErrorIs(t, sources.stage(t.Context(), staging, module, false, inputs), ErrFrontendInputsChanged)
	require.NoError(t, os.Remove(file))
	require.ErrorIs(t, sources.stage(t.Context(), staging, module, false, inputs), ErrFrontendInputsChanged)
}

func TestFrontendWatchRetainsIgnoredInputsAndBrokenGraphRepair(t *testing.T) {
	root, module := wikiRepository(t)
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	tracked := filepath.Join(remote, "src", "nested", "Widget.tsx")
	vendor := filepath.Join(root, "vendor", "shared.tgz")
	require.NoError(t, os.MkdirAll(filepath.Dir(vendor), 0755))
	require.NoError(t, os.WriteFile(vendor, []byte("tarball v1"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("vendor/\nsolutions/wiki/services/fe-remote/src/\n"), 0600))
	manifest := filepath.Join(remote, "package.json")
	require.NoError(t, os.WriteFile(manifest, []byte(`{"dependencies":{"shared":"file:../../../../vendor/shared.tgz"}}`), 0600))
	tree, files, err := FrontendWatchInterest(t.Context(), module)
	require.NoError(t, err)
	require.Len(t, files, 2)
	for _, dir := range []string{filepath.Dir(vendor), filepath.Dir(tracked)} {
		physical, err := filepath.EvalSymlinks(dir)
		require.NoError(t, err)
		require.False(t, tree.Exclude(physical), "ignored inputs are not unconditional output")
	}
	require.True(t, tree.Exclude(filepath.Join(tree.Root, "solutions/wiki/services/fe-remote/dist")))
	dependencies := builders.NewDependencies("frontend")
	for _, file := range files {
		dependencies.AddDependencies(builders.NewDependency(file))
	}
	events := make(chan code.Change, 100)
	watcher, err := code.NewWatcher(t.Context(), events, tree.Root, dependencies, tree)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go watcher.Start(ctx)
	build := LocalFrontend{}
	for _, file := range []string{tracked, vendor} {
		before, err := build.InputDigest(ctx, module)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(file, []byte("changed"), 0600))
		relative, err := filepath.Rel(root, file)
		require.NoError(t, err)
		awaitFrontendEvent(t, events, relative)
		after, err := build.InputDigest(ctx, module)
		require.NoError(t, err)
		require.NotEqual(t, before, after)
	}
	// Invalid metadata does not change or discard the previously registered interest.
	require.NoError(t, os.WriteFile(manifest, []byte(`{"dependencies":{"missing":"file:../missing"}}`), 0600))
	_, err = build.InputDigest(ctx, module)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(manifest, []byte(`{}`), 0600))
	awaitFrontendEvent(t, events, "solutions/wiki/services/fe-remote/package.json")
	_, err = build.InputDigest(ctx, module)
	require.NoError(t, err)
	// Index/exclude live outside the traversed tree in a Git worktree too.
	require.NoError(t, os.WriteFile(files[1], []byte("other-ignored\n"), 0600))
	awaitFrontendEvent(t, events, filepath.Join(".git", "info", "exclude"))
}

func awaitFrontendEvent(t *testing.T, events <-chan code.Change, path string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case change := <-events:
			require.NoError(t, change.Err)
			if change.Path == path {
				return
			}
		case <-deadline:
			t.Fatalf("selected frontend input event not observed: %s", path)
		}
	}
}

func TestLocalFrontendRejectsEditDuringRealBuildAndRepairs(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm unavailable; real build not qualified")
	}
	t.Setenv("npm_config_cache", t.TempDir())
	t.Setenv("npm_config_offline", "true")
	t.Setenv("npm_config_audit", "false")
	t.Setenv("npm_config_fund", "false")
	barriers := t.TempDir()
	ready, _ := json.Marshal(filepath.Join(barriers, "ready"))
	release, _ := json.Marshal(filepath.Join(barriers, "release"))
	module := frontendModule(t, map[string]string{
		"package.json": `{"private":true,"scripts":{"build":"node build.cjs"}}`,
		"Page.tsx":     "before",
		"build.cjs": `const fs=require('fs'); fs.writeFileSync(` + string(ready) + `, 'ready');
const wait=()=>{if(!fs.existsSync(` + string(release) + `)){setTimeout(wait,10);return;}
fs.mkdirSync('services/fe-remote/dist',{recursive:true});
fs.writeFileSync('services/fe-remote/dist/mf-manifest.json',fs.readFileSync('Page.tsx'));};wait();`,
	})
	var retained atomic.Int32
	build := LocalFrontend{StateDir: t.TempDir(), Retained: func(string) bool { return true }, Retain: func(_ context.Context, root string) (string, error) {
		retained.Add(1)
		data, err := os.ReadFile(filepath.Join(root, FrontendBundleManifest))
		return string(data), err
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := build.Bundle(ctx, module); done <- err }()
	require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(barriers, "ready")); return err == nil }, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), "Page.tsx"), []byte("after"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(barriers, "release"), nil, 0600))
	require.ErrorIs(t, <-done, ErrFrontendInputsChanged)
	require.Zero(t, retained.Load(), "stale build must not be retained as the current input snapshot")
	bundle, err := build.Bundle(ctx, module)
	require.NoError(t, err)
	require.Equal(t, "after", bundle)
	require.EqualValues(t, 1, retained.Load())
}
