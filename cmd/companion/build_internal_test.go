package companion

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/companions"
	"github.com/stretchr/testify/require"
)

// A companion whose spec names a CLI binary expects it staged in the context
// before the build; one that names none takes its CLI from the base image with
// COPY --from and needs nothing staged.
func TestNeedsLinuxCLIFollowsTheDeclaredCLIBinary(t *testing.T) {
	specs, err := loadBuildSpecs()
	require.NoError(t, err)

	require.True(t, needsLinuxCLI(specs, []*Companion{{Name: "execution"}}),
		"the execution companion bakes a freshly built linux CLI")
	require.False(t, needsLinuxCLI(specs, []*Companion{{Name: "go"}}),
		"a language companion copies the CLI out of the base image, not the context")
	require.True(t, needsLinuxCLI(specs, []*Companion{{Name: "go"}, {Name: baseCompanionName}}),
		"one target that bakes the CLI is enough to have to stage it")
}

// core declares the Dockerfile and the context every companion image is built
// from. The spec is deliberately given values the old name heuristic would
// never produce — it derived the Dockerfile from the companion name and always
// built from the core root — so a build that ignored the spec and inferred the
// paths again fails here.
func TestBuildWithDockerBuildsWhatTheSpecDeclares(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "proto", "0.0.11", true, false)

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf("echo \"$@\" >> %q\nexit 0\n", dockerLog))

	spec := companions.BuildSpec{
		Name:       "proto",
		Dockerfile: "build/proto.Dockerfile",
		Context:    "companions/proto",
	}
	c, err := LoadCompanion(filepath.Join(root, "companions", "proto"))
	require.NoError(t, err)
	_, err = buildWithDocker(c, spec, root, false, []dockerPlatform{{Value: "linux/amd64", Arch: "amd64"}}, false, "")
	require.NoError(t, err)

	logged := strings.TrimSpace(string(readFile(t, dockerLog)))
	require.Contains(t, logged, "-f "+filepath.FromSlash(spec.Dockerfile))
	require.NotContains(t, logged, filepath.Join("companions", "proto", "Dockerfile"),
		"the Dockerfile must come from the spec, not from the companion name")
	require.True(t, strings.HasSuffix(logged, " "+filepath.FromSlash(spec.Context)),
		"the build context must be the one the spec declares; got %q", logged)
}

// A Dockerfile core declares no spec for has no Dockerfile path and no context
// to build it with. The run stops before anything is built rather than
// publishing the companions ordered ahead of it and failing on this one, which
// reads as a partial-publish incident instead of a stale core pin.
func TestBuildTargetsRefusesTheWholeRunForACompanionCoreDoesNotDeclare(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "invented", "0.0.1", true, false)

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf("echo \"$@\" >> %q\nexit 0\n", dockerLog))

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	_, err = buildTargets(root, targets, BuildOptions{Push: true})
	require.ErrorContains(t, err, "no build spec for invented")

	_, readErr := os.Stat(dockerLog)
	require.True(t, os.IsNotExist(readErr),
		"an undeclared companion must stop the run before anything is built or pushed")
}

// A companion whose spec names no base but whose Dockerfile takes one gets no
// --build-arg, so the Dockerfile's pinned ARG default wins and the image
// publishes built on whatever that stale tag names. The specs come from the
// pinned core module and the Dockerfile from --core-dir, so the two disagree
// exactly when the CLI's pin trails the tree being built.
func TestBuildTargetsRefusesWhenTheTreeTakesABaseTheSpecDoesNotDeclare(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, "execution", "0.0.2", true, false)
	writeBaseDependentDockerfile(t, root, "execution", "ghcr.io/codefly-dev/codefly:0.0.4")

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf("echo \"$@\" >> %q\nexit 0\n", dockerLog))

	execution, err := LoadCompanion(filepath.Join(root, "companions", "execution"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{execution}, BuildOptions{})
	require.ErrorContains(t, err, companions.BaseImageArg)
	require.ErrorContains(t, err, "names no base companion")

	_, readErr := os.Stat(dockerLog)
	require.True(t, os.IsNotExist(readErr),
		"a base the spec and the tree disagree on must stop the run before anything is built")
}

// The mirror image: the spec names a base the Dockerfile has no argument to
// receive, so the resolved reference is passed and ignored and the image is
// built on whatever its own FROM says instead.
func TestBuildTargetsRefusesWhenTheSpecNamesABaseTheTreeCannotReceive(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeFakeDocker(t, "exit 0\n")

	dependent, err := LoadCompanion(filepath.Join(root, "companions", "go"))
	require.NoError(t, err)

	_, err = buildTargets(root, []*Companion{dependent}, BuildOptions{})
	require.ErrorContains(t, err, "declares no "+companions.BaseImageArg)
}

// withFastRegistryRead shrinks the digest-lookup retry delay so tests that
// exhaust the retries don't spend real wall-clock time waiting on it.
func withFastRegistryRead(t *testing.T) {
	t.Helper()
	original := registryReadDelay
	registryReadDelay = time.Millisecond
	t.Cleanup(func() { registryReadDelay = original })
}

// The base a run does not push exists only in the local daemon, which the
// registry has no digest for; a lookup here would exit non-zero, so this also
// asserts none runs.
func TestResolveBaseImage_KeepsTheTagWhenTheRunDoesNotPush(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.4", true, false)
	writeFakeDocker(t, "exit 1\n")

	ref, err := resolveBaseImage(root, false, "")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/codefly-dev/codefly:0.0.4", ref)
}

// The digest this run pushed is the only source that cannot be overtaken, so a
// lookup must not happen at all when there is one. The fake docker fails every
// invocation: reaching the registry here is itself the failure.
func TestResolveBaseImage_PrefersTheDigestThisRunPushed(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.4", true, false)
	writeFakeDocker(t, "exit 1\n")

	ref, err := resolveBaseImage(root, true, "sha256:"+pushedBaseDigest)
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest, ref)
}

// With no digest of its own — the base was published by an earlier run — the
// tag is all there is to go on, and reading it is correct.
func TestResolveBaseImage_ReadsTheRegistryWhenThisRunDidNotPushTheBase(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.4", true, false)
	writeFakeDocker(t, "echo sha256:"+predecessorBaseDigest+"\nexit 0\n")

	ref, err := resolveBaseImage(root, true, "")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/codefly-dev/codefly@sha256:"+predecessorBaseDigest, ref)
}

// This lookup sits between a published base and its dependents, inside a run
// that has already mutated the registry. One 5xx must not abort it.
func TestImageDigest_RetriesATransientRegistryFailure(t *testing.T) {
	withFastRegistryRead(t)
	attempts := filepath.Join(t.TempDir(), "attempts")
	writeFakeDocker(t, fmt.Sprintf(`
echo x >> %[1]q
if [ "$(wc -l < %[1]q)" -lt 2 ]; then
  echo "received unexpected HTTP status: 503 Service Unavailable" 1>&2
  exit 1
fi
echo sha256:%[2]s
exit 0
`, attempts, pushedBaseDigest))

	digest, err := imageDigest("ghcr.io/codefly-dev/codefly:0.0.4")
	require.NoError(t, err, "a transient registry failure must not abort a half-published run")
	require.Equal(t, "sha256:"+pushedBaseDigest, digest)
}

// buildx renders a field it cannot resolve as the empty string and still exits
// 0. Accepting that composes a --build-arg ending in a bare "@", which reads as
// a reference and resolves to nothing.
func TestImageDigest_RejectsOutputThatIsNotADigest(t *testing.T) {
	withFastRegistryRead(t)
	writeFakeDocker(t, "echo ''\nexit 0\n")

	_, err := imageDigest("ghcr.io/codefly-dev/codefly:0.0.4")
	require.ErrorContains(t, err, "not a digest")
}

func TestImageDigest_ReportsWhatDockerWroteToStderr(t *testing.T) {
	withFastRegistryRead(t)
	writeFakeDocker(t, `
echo "ghcr.io/codefly-dev/codefly:0.0.4: not found" 1>&2
exit 1
`)

	_, err := imageDigest("ghcr.io/codefly-dev/codefly:0.0.4")
	require.ErrorContains(t, err, "not found")
}

// A dependent must be pinned to the image this run put in the registry, taken
// from the push itself. Reading the tag back would answer with whatever it
// names at that moment, which anything with write access can change. The fake
// docker fails every imagetools call, so a read-back fails the test outright.
func TestBuildTargets_PinsDependentsToTheDigestReportedByThePush(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf(`
echo "$@" >> %[1]q
case "$1" in
  push)   echo "0.0.5: digest: sha256:%[2]s size: 1234"; exit 0 ;;
  buildx) echo "the tag must not be read back for a base this run pushed" 1>&2; exit 1 ;;
  manifest) echo '{}'; exit 0 ;;
esac
exit 0
`, dockerLog, pushedBaseDigest))

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	published, err := buildTargets(root, targets, BuildOptions{Push: true})
	require.NoError(t, err)
	require.Len(t, published, 2)

	logged, err := os.ReadFile(dockerLog)
	require.NoError(t, err)
	require.NotContains(t, string(logged), "imagetools",
		"the digest comes from the push, not from re-reading the tag")
	require.Contains(t, dependentBuildLine(t, logged, "go"),
		"--build-arg "+companions.BaseImageArg+"=ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest)

	// The base a dependent consumed cannot be recovered after the run: the tag
	// it was published under is mutable, so it is recorded here or nowhere.
	byName := map[string]publishedImage{}
	for _, p := range published {
		byName[p.Companion.Name] = p
	}
	require.Equal(t, "sha256:"+pushedBaseDigest, byName[baseCompanionName].Digest)
	require.Empty(t, byName[baseCompanionName].Base, "the base builds on no companion")
	require.Equal(t, "ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest, byName["go"].Base)
}

// The multi-platform path is what CI publishes with, and it never reaches
// pushImage: buildx uploads the manifest list itself. The digest it records for
// that push is the only report of what landed, so the base's dependents are
// pinned from the metadata file rather than from a read-back.
func TestBuildTargets_PinsDependentsToTheDigestBuildxPushed(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf(`
echo "$@" >> %[1]q
if [ "$1 $2" = "buildx imagetools" ]; then
  echo "the tag must not be read back for a base this run pushed" 1>&2
  exit 1
fi
prev=""
for arg in "$@"; do
  if [ "$prev" = "--metadata-file" ]; then
    printf '{"containerimage.digest":"sha256:%[2]s"}' > "$arg"
  fi
  prev="$arg"
done
exit 0
`, dockerLog, pushedBaseDigest))

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	published, err := buildTargets(root, targets, BuildOptions{Push: true, Platform: "linux/amd64,linux/arm64"})
	require.NoError(t, err)
	require.Len(t, published, 2)

	logged := readFile(t, dockerLog)
	require.NotContains(t, string(logged), "docker push",
		"buildx pushes the manifest list itself")
	require.Contains(t, dependentBuildLine(t, logged, "go"),
		"--build-arg "+companions.BaseImageArg+"=ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest)
}

// A run that was publishing the base and failed to get it into the registry
// owes its dependents nothing else. Falling back to the tag would build them on
// the previous base — published by a run that never built these dependents —
// and report success.
func TestBuildTargets_RefusesToPinDependentsWhenTheBasePushFailed(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")
	writeFakeDocker(t, `
case "$1" in
  push) echo "error: failed to do request: connection reset by peer" 1>&2; exit 1 ;;
  buildx) echo sha256:`+predecessorBaseDigest+`; exit 0 ;;
esac
exit 0
`)

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	_, err = buildTargets(root, targets, BuildOptions{Push: true})
	require.Error(t, err)
	require.ErrorContains(t, err, "did not put in the registry")
}

// The base's upload landing is what dependents need; the visibility check that
// follows is a separate failure, still collected. A first publish of a new
// package fails that check by design, and stranding every dependent on it would
// make the whole set unpublishable in exactly that run.
func TestBuildTargets_PinsDependentsWhenTheBasePushedButIsNotYetPublic(t *testing.T) {
	withFastPushVerifyRetry(t)
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf(`
echo "$@" >> %[1]q
case "$1" in
  push)     echo "0.0.5: digest: sha256:%[2]s size: 1234"; exit 0 ;;
  manifest) echo "denied: requested access to the resource is denied" 1>&2; exit 1 ;;
esac
exit 0
`, dockerLog, pushedBaseDigest))

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	_, err = buildTargets(root, targets, BuildOptions{Push: true})
	require.ErrorContains(t, err, "not publicly pullable")
	require.Contains(t, dependentBuildLine(t, readFile(t, dockerLog), "go"),
		"--build-arg "+companions.BaseImageArg+"=ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest)
}

// The argument goes to the companions whose Dockerfile declares it and to no
// others. Passing it to every non-base companion leaves an unconsumed build-arg
// on each one that takes no base.
func TestBuildTargets_PassesTheBaseOnlyToCompanionsThatDeclareIt(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")
	writeManifest(t, root, "execution", "0.0.2", true, false)

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf(`
echo "$@" >> %[1]q
case "$1" in
  push)     echo "x: digest: sha256:%[2]s size: 1234"; exit 0 ;;
  manifest) echo '{}'; exit 0 ;;
esac
exit 0
`, dockerLog, pushedBaseDigest))

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	_, err = buildTargets(root, targets, BuildOptions{Push: true})
	require.NoError(t, err)

	logged := readFile(t, dockerLog)
	require.Contains(t, dependentBuildLine(t, logged, "go"), companions.BaseImageArg+"=")
	require.NotContains(t, dependentBuildLine(t, logged, "execution"), companions.BaseImageArg,
		"a companion that declares no base takes no base argument")
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// dependentBuildLine returns the recorded `docker build` invocation for one
// companion, so an assertion names the arguments that companion was built with
// rather than searching the whole log.
func dependentBuildLine(t *testing.T, log []byte, name string) string {
	t.Helper()
	needle := filepath.Join("companions", name, "Dockerfile")
	for _, line := range strings.Split(string(log), "\n") {
		if strings.Contains(line, "build ") && strings.Contains(line, needle) {
			return line
		}
	}
	require.Failf(t, "no build recorded", "companion %q not built; docker invocations: %s", name, log)
	return ""
}

const (
	pushedBaseDigest      = "1111111111111111111111111111111111111111111111111111111111111111"
	predecessorBaseDigest = "2222222222222222222222222222222222222222222222222222222222222222"
)
