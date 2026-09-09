package companion

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A tag already in the registry is one agents have resolved; republishing it
// swaps the image under every consumer without changing the reference they
// pin. Publishing must skip it and say a version bump is how a change ships.
func TestAlreadyPublished_SkipsConfirmedTags(t *testing.T) {
	writeFakeDocker(t, `
echo '{"schemaVersion":2}'
exit 0
`)
	root := t.TempDir()
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	alpha, err := LoadCompanion(filepath.Join(root, "companions", "alpha"))
	require.NoError(t, err)

	published, pending := alreadyPublished([]*Companion{alpha})
	require.Len(t, published, 1)
	require.Empty(t, pending, "a tag present in the registry must not be rebuilt over")
}

// A ghcr package that has never been pushed answers "denied", not "manifest
// unknown". Treating that as published would make the first publish of a new
// companion impossible, so only a confirmed-present tag is skipped.
func TestAlreadyPublished_TreatsUnresolvableTagsAsPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	writeManifest(t, root, "beta", "0.0.2", true, false)
	alpha, err := LoadCompanion(filepath.Join(root, "companions", "alpha"))
	require.NoError(t, err)
	beta, err := LoadCompanion(filepath.Join(root, "companions", "beta"))
	require.NoError(t, err)

	for name, response := range map[string]string{
		"never pushed (package absent)": `echo "denied: requested access to the resource is denied" 1>&2; exit 1`,
		"absent tag":                    `echo "manifest unknown" 1>&2; exit 1`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFakeDocker(t, response+"\n")
			published, pending := alreadyPublished([]*Companion{alpha, beta})
			require.Empty(t, published)
			require.Len(t, pending, 2)
		})
	}
}

// The base is the only build whose failure invalidates what follows, so a
// leaf that fails to build must not strand the companions ordered after it —
// the guarantee core's publish matrix gave with fail-fast: false.
func TestBuildTargets_LeafBuildFailureDoesNotStrandTheRest(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "broken", "0.0.1", true, false)
	writeManifest(t, root, "healthy", "0.0.2", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then
  for arg in "$@"; do
    case "$arg" in
      *broken*) exit 1 ;;
    esac
  done
  echo "$@" >> %q
fi
exit 0
`, buildLog))

	broken, err := LoadCompanion(filepath.Join(root, "companions", "broken"))
	require.NoError(t, err)
	healthy, err := LoadCompanion(filepath.Join(root, "companions", "healthy"))
	require.NoError(t, err)

	err = buildTargets(root, []*Companion{broken, healthy}, BuildOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "broken")

	built, readErr := os.ReadFile(buildLog)
	require.NoError(t, readErr)
	require.Contains(t, string(built), healthy.Tag(),
		"a leaf build failure must not abort the companions ordered after it")
}

// writeSiblingCLI creates the cli/ checkout buildLinuxCLI cross-compiles from.
// Companions that bake the CLI (codefly, execution, the language runtimes)
// stage a linux binary before Docker runs, and that step resolves ../cli
// relative to the core directory.
func writeSiblingCLI(t *testing.T, coreDir string) {
	t.Helper()
	dir := filepath.Join(coreDir, "..", "cli")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module fake/cli\n\ngo 1.25\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o600))
}

// Everything else resolves the base as its FROM, so continuing past a failed
// base build would publish dependents against a stale or missing image.
func TestBuildTargets_BaseBuildFailureAbortsTheRun(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "codefly", "0.0.1", true, false)
	writeManifest(t, root, "healthy", "0.0.2", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then
  for arg in "$@"; do
    case "$arg" in
      *codefly:*) exit 1 ;;
    esac
  done
  echo "$@" >> %q
fi
exit 0
`, buildLog))

	base, err := LoadCompanion(filepath.Join(root, "companions", "codefly"))
	require.NoError(t, err)
	healthy, err := LoadCompanion(filepath.Join(root, "companions", "healthy"))
	require.NoError(t, err)

	err = buildTargets(root, []*Companion{base, healthy}, BuildOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "every other companion builds on it")

	_, readErr := os.ReadFile(buildLog)
	require.True(t, os.IsNotExist(readErr), "nothing may be built after the base fails")
}

// The abort rule and the build order encode the same fact; if they disagree,
// a companion others build on could be ordered after them or treated as a
// leaf whose failure is survivable.
func TestBaseCompanionIsTheOneOrderedFirst(t *testing.T) {
	ordered := sortCompanionsForBuild([]*Companion{
		{Name: "python"}, {Name: "proto"}, {Name: "codefly"}, {Name: "execution"},
	})
	require.True(t, isBaseCompanion(ordered[0].Name),
		"the companion built first must be the one the abort rule treats as the base")
	for _, c := range ordered[1:] {
		require.False(t, isBaseCompanion(c.Name), "%s is ordered after the base but treated as one", c.Name)
	}
}

// Wiring: publish must consult the registry before building. Without this the
// skip helper can exist and still never run, which is how core's deleted
// workflow's immutability guarantee would silently stay lost.
func TestRunPublish_DoesNotBuildAnAlreadyPublishedTag(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "alpha", "0.0.1", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then echo "$@" >> %q; fi
echo '{"schemaVersion":2}'
exit 0
`, buildLog))

	require.NoError(t, PublishCmd.Flags().Set("core-dir", root))
	t.Cleanup(func() { _ = PublishCmd.Flags().Set("core-dir", "") })

	require.NoError(t, runPublish(PublishCmd, []string{"alpha"}))
	_, err := os.ReadFile(buildLog)
	require.True(t, os.IsNotExist(err), "an already-published tag must not be rebuilt and pushed over")
}
