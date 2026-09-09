package companion

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/companions"
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

	published, pending, err := alreadyPublished([]*Companion{alpha})
	require.NoError(t, err)
	require.Len(t, published, 1)
	require.Empty(t, pending, "a tag present in the registry must not be rebuilt over")
}

// A private package that already exists resolves for an authenticated caller
// and must be skipped like any other published tag. Republishing it would
// overwrite the image and still leave it private — visibility is a registry
// setting, not something a push changes.
func TestAlreadyPublished_SkipsExistingPrivatePackages(t *testing.T) {
	// The fake docker below distinguishes the anonymous probe from the
	// authenticated one by DOCKER_CONFIG; pin it so an operator who exports it
	// does not change what this test asserts.
	t.Setenv("DOCKER_CONFIG", "")
	writeFakeDocker(t, `
if [ "$1" = "manifest" ] && [ -n "$DOCKER_CONFIG" ]; then
  echo "denied: requested access to the resource is denied" 1>&2
  exit 1
fi
echo '{"schemaVersion":2}'
exit 0
`)
	root := t.TempDir()
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	alpha, err := LoadCompanion(filepath.Join(root, "companions", "alpha"))
	require.NoError(t, err)

	published, pending, err := alreadyPublished([]*Companion{alpha})
	require.NoError(t, err, "an authenticated lookup resolves a private package that exists")
	require.Len(t, published, 1)
	require.Empty(t, pending)
}

// A tag the registry confirms is absent is the one case that may be built
// over, and it must stay buildable or the first publish of a new companion
// becomes impossible.
func TestAlreadyPublished_TreatsConfirmedAbsentTagsAsPending(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	writeManifest(t, root, "beta", "0.0.2", true, false)
	alpha, err := LoadCompanion(filepath.Join(root, "companions", "alpha"))
	require.NoError(t, err)
	beta, err := LoadCompanion(filepath.Join(root, "companions", "beta"))
	require.NoError(t, err)

	writeFakeDocker(t, `echo "manifest unknown" 1>&2; exit 1`+"\n")
	published, pending, err := alreadyPublished([]*Companion{alpha, beta})
	require.NoError(t, err)
	require.Empty(t, published)
	require.Len(t, pending, 2)
}

// A lookup that fails for any reason other than a confirmed-absent manifest
// answers neither "published" nor "not published". Folding it into "not
// published" is how a network blip, a registry 5xx or a missing login silently
// overwrites every live tag in the fleet — the exact loss this command exists
// to prevent — so it must stop the run instead.
func TestAlreadyPublished_RefusesWhenTheLookupCannotDecide(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	alpha, err := LoadCompanion(filepath.Join(root, "companions", "alpha"))
	require.NoError(t, err)

	for name, response := range map[string]string{
		"not logged in":        `echo "denied: requested access to the resource is denied" 1>&2; exit 1`,
		"registry unreachable": `echo "dial tcp: lookup ghcr.io: no such host" 1>&2; exit 1`,
		"rate limited":         `echo "toomanyrequests: retry later" 1>&2; exit 1`,
	} {
		t.Run(name, func(t *testing.T) {
			writeFakeDocker(t, response+"\n")
			published, pending, err := alreadyPublished([]*Companion{alpha})
			require.Error(t, err, "an undecidable lookup must not be read as \"safe to overwrite\"")
			require.Contains(t, err.Error(), "cannot tell whether")
			require.Empty(t, published)
			require.Empty(t, pending)
		})
	}
}

// The refusal above is only worth anything if it actually stops the build.
func TestRunPublish_DoesNotBuildWhenTheRegistryLookupFails(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "alpha", "0.0.1", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then echo "$@" >> %q; exit 0; fi
echo "denied: requested access to the resource is denied" 1>&2
exit 1
`, buildLog))

	require.NoError(t, PublishCmd.Flags().Set("core-dir", root))
	t.Cleanup(func() { _ = PublishCmd.Flags().Set("core-dir", "") })

	err := runPublish(PublishCmd, []string{"alpha"})
	require.Error(t, err)
	_, statErr := os.ReadFile(buildLog)
	require.True(t, os.IsNotExist(statErr), "nothing may be built over a tag whose state is unknown")
}

// The base is the only build whose failure invalidates what follows, so a
// leaf that fails to build must not strand the companions ordered after it —
// the guarantee core's publish matrix gave with fail-fast: false.
func TestBuildTargets_LeafBuildFailureDoesNotStrandTheRest(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.4", true, false)
	writeManifest(t, root, "proto", "0.0.1", true, false)
	writeManifest(t, root, "python", "0.0.2", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then
  for arg in "$@"; do
    case "$arg" in
      *proto*) exit 1 ;;
    esac
  done
  echo "$@" >> %q
fi
exit 0
`, buildLog))

	broken, err := LoadCompanion(filepath.Join(root, "companions", "proto"))
	require.NoError(t, err)
	healthy, err := LoadCompanion(filepath.Join(root, "companions", "python"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{broken, healthy}, BuildOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "proto")

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

	_, err = buildTargets(root, []*Companion{base, healthy}, BuildOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "every other companion builds on it")

	_, readErr := os.ReadFile(buildLog)
	require.True(t, os.IsNotExist(readErr), "nothing may be built after the base fails")
}

// The abort rule and the build order encode the same fact; if they disagree,
// a companion others build on could be ordered after them or treated as a
// leaf whose failure is survivable.
func TestBaseCompanionIsTheOneOrderedFirst(t *testing.T) {
	ordered, err := sortCompanionsForBuild([]*Companion{
		{Name: "python"}, {Name: "proto"}, {Name: "codefly"}, {Name: "execution"},
	})
	require.NoError(t, err)
	require.True(t, isBaseCompanion(ordered[0].Name),
		"the companion built first must be the one the abort rule treats as the base")
	for _, c := range ordered[1:] {
		require.False(t, isBaseCompanion(c.Name), "%s is ordered after the base but treated as one", c.Name)
	}
}

// publishedAll runs `publish --all` against root with a fake docker already in
// place, restoring the shared command flags afterwards.
func publishedAll(t *testing.T, root string, extra map[string]string) error {
	t.Helper()
	set := map[string]string{"core-dir": root, "all": "true"}
	for k, v := range extra {
		set[k] = v
	}
	for k, v := range set {
		require.NoError(t, PublishCmd.Flags().Set(k, v))
	}
	// PublishCmd's flags are package-level and shared by every test in this
	// file, so each one must restore its own. A bool reset with "" fails to
	// parse and silently leaves the flag set for whatever runs next.
	t.Cleanup(func() {
		for k := range set {
			def := ""
			if f := PublishCmd.Flags().Lookup(k); f != nil && f.Value.Type() == "bool" {
				def = "false"
			}
			require.NoError(t, PublishCmd.Flags().Set(k, def))
		}
	})
	return runPublish(PublishCmd, nil)
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

	require.NoError(t, publishedAll(t, root, nil))
	_, err := os.ReadFile(buildLog)
	require.True(t, os.IsNotExist(err), "an already-published tag must not be rebuilt and pushed over")
}

// Skipping is right for a set, but an operator who named one companion asked
// for that image to be published and would read exit 0 as "it was". Same
// reasoning as the existing no-image guard one branch above it.
func TestRunPublish_NamedAlreadyPublishedTargetFailsLoudly(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	writeFakeDocker(t, "echo '{\"schemaVersion\":2}'\nexit 0\n")

	require.NoError(t, PublishCmd.Flags().Set("core-dir", root))
	t.Cleanup(func() { _ = PublishCmd.Flags().Set("core-dir", "") })

	err := runPublish(PublishCmd, []string{"alpha"})
	require.Error(t, err, "naming one already-published companion must not report success")
	require.Contains(t, err.Error(), "already published")
	require.Contains(t, err.Error(), "--force")
}

// --force is the only way past the skip, so a break in its wiring leaves an
// operator with no way to republish at all. GetBool discards its error, which
// is exactly how such a break stays silent.
func TestRunPublish_ForceRepublishesAnAlreadyPublishedTag(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "execution", "0.0.1", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then echo "$@" >> %q; fi
echo '{"schemaVersion":2}'
exit 0
`, buildLog))

	require.NoError(t, publishedAll(t, root, map[string]string{"force": "true"}))
	built, err := os.ReadFile(buildLog)
	require.NoError(t, err, "--force must rebuild and overwrite the published tag")
	require.Contains(t, string(built), "execution")
}

// Provenance attestation consumes this file. It must list what the run pushed
// and nothing else: signing a tag the run skipped as already published claims
// a build that never happened, in a transparency log that cannot be retracted.
func TestRunPublish_ManifestRecordsOnlyWhatThisRunPushed(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "execution", "0.0.1", true, false)
	writeManifest(t, root, baseCompanionName, "0.0.2", true, false)
	t.Setenv("DOCKER_CONFIG", "")

	// The codefly tag already resolves in the registry; the execution one does
	// not. Only the authenticated lookup decides that — DOCKER_CONFIG marks the
	// anonymous probe pushImage runs afterwards, which must see the pushed image.
	writeFakeDocker(t, `
if [ "$1" = "manifest" ] && [ -z "$DOCKER_CONFIG" ]; then
  case "$3" in
    */codefly:*) echo '{"schemaVersion":2}'; exit 0 ;;
    *)           echo "manifest unknown" 1>&2; exit 1 ;;
  esac
fi
echo '{"schemaVersion":2}'
exit 0
`)

	manifestPath := filepath.Join(t.TempDir(), "published.json")
	require.NoError(t, publishedAll(t, root, map[string]string{"published-manifest": manifestPath}))

	raw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	var entries []publishedEntry
	require.NoError(t, json.Unmarshal(raw, &entries))
	require.Len(t, entries, 1, "a skipped companion must not appear in the attestable set")
	require.Equal(t, "execution", entries[0].Companion)
	require.Equal(t, "0.0.1", entries[0].Version)
}

// "published nothing" and "never got far enough to say" are different answers,
// and the consumer decides whether to attest based on which one it reads.
func TestRunPublish_ManifestIsWrittenEvenWhenNothingIsPublished(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "alpha", "0.0.1", true, false)
	writeFakeDocker(t, "echo '{\"schemaVersion\":2}'\nexit 0\n")

	manifestPath := filepath.Join(t.TempDir(), "published.json")
	require.NoError(t, publishedAll(t, root, map[string]string{"published-manifest": manifestPath}))

	raw, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	require.JSONEq(t, "[]", string(raw))
}

// writeBaseDependentDockerfile writes a companion Dockerfile shaped like core's
// language companions: it takes the base image as a build argument and carries
// a pinned default so a bare `docker build` still works.
func writeBaseDependentDockerfile(t *testing.T, root, name, defaultBase string) {
	t.Helper()
	body := fmt.Sprintf("ARG %s=%s\nFROM ${%s} AS codeflybase\nFROM alpine\n",
		companions.BaseImageArg, defaultBase, companions.BaseImageArg)
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "companions", name, "Dockerfile"), []byte(body), 0o600))
}

// core declares the base dependency (BuildSpec.Base) and delegates resolving it
// to this builder. Without the argument the Dockerfile's pinned ARG default
// wins, so bumping the base version publishes dependents still built on the
// previous base — and build, push and verify all report success.
func TestBuildTargets_PassesTheResolvedBaseImageToDependents(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then echo "$@" >> %q; fi
exit 0
`, buildLog))

	dependent, err := LoadCompanion(filepath.Join(root, "companions", "go"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{dependent}, BuildOptions{})
	require.NoError(t, err)

	built, err := os.ReadFile(buildLog)
	require.NoError(t, err)
	require.Contains(t, string(built), "--build-arg "+companions.BaseImageArg+"=ghcr.io/codefly-dev/codefly:0.0.5",
		"the dependent must build on the base version info.codefly.yaml pins now")
	require.NotContains(t, string(built), "codefly:0.0.4",
		"the Dockerfile's stale pinned default must never be what gets built")
}

// The base is routinely absent from targets — a publish run skips it once it
// is already published — and its dependents still have to be built against it.
// Resolving from the target list would silently fall back to the stale default
// in exactly those runs.
func TestBuildTargets_ResolvesTheBaseEvenWhenItIsNotBeingBuilt(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.7", true, false)
	writeManifest(t, root, "python", "0.0.3", true, false)
	writeBaseDependentDockerfile(t, root, "python", "ghcr.io/codefly-dev/codefly:0.0.4")

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then echo "$@" >> %q; fi
exit 0
`, buildLog))

	dependent, err := LoadCompanion(filepath.Join(root, "companions", "python"))
	require.NoError(t, err)

	// Only the dependent is a target; the base is not.
	_, err = buildTargets(root, []*Companion{dependent}, BuildOptions{})
	require.NoError(t, err)

	built, err := os.ReadFile(buildLog)
	require.NoError(t, err)
	require.Contains(t, string(built), companions.BaseImageArg+"=ghcr.io/codefly-dev/codefly:0.0.7")
}

// A dependent that cannot resolve its base must stop, not quietly build on
// whatever literal the Dockerfile was last edited against.
func TestBuildTargets_FailsWhenADependentsBaseCannotBeResolved(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "node", "0.0.3", true, false)
	writeBaseDependentDockerfile(t, root, "node", "ghcr.io/codefly-dev/codefly:0.0.4")
	writeFakeDocker(t, "exit 0\n")

	dependent, err := LoadCompanion(filepath.Join(root, "companions", "node"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{dependent}, BuildOptions{})
	require.Error(t, err, "an unresolvable base must not fall through to the Dockerfile default")
	require.Contains(t, err.Error(), baseCompanionName)
}

// The base builds itself, and a companion whose spec names no base takes none —
// passing one would be an unconsumed build-arg on every such image.
func TestBuildTargets_PassesNoBaseArgumentWhereNoneIsDeclared(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "execution", "0.0.1", true, false)

	buildLog := filepath.Join(t.TempDir(), "built.txt")
	writeFakeDocker(t, fmt.Sprintf(`
if [ "$1" = "build" ]; then echo "$@" >> %q; fi
exit 0
`, buildLog))

	base, err := LoadCompanion(filepath.Join(root, "companions", baseCompanionName))
	require.NoError(t, err)
	execution, err := LoadCompanion(filepath.Join(root, "companions", "execution"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{base, execution}, BuildOptions{})
	require.NoError(t, err)

	built, err := os.ReadFile(buildLog)
	require.NoError(t, err)
	require.NotContains(t, string(built), companions.BaseImageArg)
}
