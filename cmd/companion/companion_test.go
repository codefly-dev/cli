package companion_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/cmd/companion"
)

// writeCompanion materializes a single companion directory at root.
// dockerfile / flake control which presence flags are set; pass empty
// to skip the file. Returns the companion's absolute path.
func writeCompanion(t *testing.T, root, name, version string, dockerfile, flake bool) string {
	t.Helper()
	dir := filepath.Join(root, "companions", name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info.codefly.yaml"),
		[]byte("version: "+version+"\n"), 0o600))
	if dockerfile {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"),
			[]byte("FROM alpine:3.21\n"), 0o600))
	}
	if flake {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "flake.nix"),
			[]byte("{ description = \"x\"; outputs = _: {}; }\n"), 0o600))
	}
	return dir
}

func TestLoadCompanion_ParsesVersionAndDetectsBuildFiles(t *testing.T) {
	root := t.TempDir()
	dir := writeCompanion(t, root, "proto", "1.2.3", true, true)

	c, err := companion.LoadCompanion(dir)
	require.NoError(t, err)
	require.Equal(t, "proto", c.Name)
	require.Equal(t, "1.2.3", c.Info.Version)
	require.True(t, c.HasDockerfile)
	require.True(t, c.HasFlake)
	require.Equal(t, "codeflydev/proto:1.2.3", c.Tag())
}

func TestLoadCompanion_FailsLoudWhenManifestMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := companion.LoadCompanion(dir)
	require.Error(t, err, "missing info.codefly.yaml must be a load error, not a silent zero-companion")
}

func TestLoadCompanion_FailsLoudWhenVersionEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info.codefly.yaml"),
		[]byte("version:\n"), 0o600))
	_, err := companion.LoadCompanion(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "version field is required")
}

func TestListCompanions_DiscoversAndSkipsNonCompanionDirs(t *testing.T) {
	root := t.TempDir()
	writeCompanion(t, root, "go", "0.0.1", true, false)
	writeCompanion(t, root, "python", "0.0.2", true, false)
	writeCompanion(t, root, "proto", "0.0.3", true, true)

	// Add a non-companion dir under companions/ — it should be
	// silently skipped (no info.codefly.yaml = not a companion).
	require.NoError(t, os.MkdirAll(filepath.Join(root, "companions", "scripts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "companions", "scripts", "build.sh"),
		[]byte("#!/bin/bash\n"), 0o755))

	cs, err := companion.ListCompanions(root)
	require.NoError(t, err)
	require.Len(t, cs, 3, "should find exactly three companions; non-companion dir must not appear")

	names := make(map[string]string, len(cs))
	for _, c := range cs {
		names[c.Name] = c.Info.Version
	}
	require.Equal(t, "0.0.1", names["go"])
	require.Equal(t, "0.0.2", names["python"])
	require.Equal(t, "0.0.3", names["proto"])
}

func TestListCompanions_ReturnsNilWhenNoCompanionsDir(t *testing.T) {
	// No companions/ subdirectory at all. Must not error — this is
	// the "wrong directory, just bail" path that lets ListCmd print
	// a friendly message.
	cs, err := companion.ListCompanions(t.TempDir())
	require.NoError(t, err)
	require.Nil(t, cs)
}

func TestFindCompanionsRoot_WalksUpward(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "companions", "proto"), 0o755))

	// Start from a deep subdirectory; FindCompanionsRoot should walk
	// up to root.
	deep := filepath.Join(root, "companions", "proto", "deeper", "still")
	require.NoError(t, os.MkdirAll(deep, 0o755))
	got := companion.FindCompanionsRoot(deep)

	// EvalSymlinks because t.TempDir on macOS returns /var/folders/...
	// while filepath.Abs returns /private/var/folders/... — the two
	// resolve to the same inode but compare unequal as strings.
	gotResolved, err := filepath.EvalSymlinks(got)
	require.NoError(t, err)
	rootResolved, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, rootResolved, gotResolved,
		"FindCompanionsRoot must return the directory containing companions/, walking up if needed")
}

func TestFindCompanionsRoot_FallsBackWhenNoAncestor(t *testing.T) {
	// No companions/ anywhere on the path. Must return the start
	// directory; the caller's stat check on companions/ will then
	// fail loudly rather than us returning a misleading root.
	dir := t.TempDir()
	got := companion.FindCompanionsRoot(dir)
	require.Equal(t, dir, got,
		"with no companions/ on the path, FindCompanionsRoot must return its input")
}
