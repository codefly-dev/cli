package companion

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNeedsLinuxCLIIncludesExecutionCompanion(t *testing.T) {
	if !needsLinuxCLI([]*Companion{{Name: "execution"}}) {
		t.Fatal("execution companion must embed a freshly built linux CLI")
	}
}

// The base a run does not push exists only in the local daemon, which the
// registry has no digest for; a lookup here would exit non-zero, so this also
// asserts none runs.
func TestResolveBaseImage_KeepsTheTagWhenTheRunDoesNotPush(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.4", true, false)
	writeFakeDocker(t, "exit 1\n")

	ref, err := resolveBaseImage(root, false)
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/codefly-dev/codefly:0.0.4", ref)
}

// A dependent of a pushed base resolves it out of the registry, where the tag
// is mutable: anything that moves the tag between the base push and the
// dependent build changes what the dependent bakes in. The digest does not
// move, so pin to it.
func TestResolveBaseImage_PinsThePushedBaseByDigest(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, baseCompanionName, "0.0.4", true, false)
	writeFakeDocker(t, "echo sha256:"+pushedBaseDigest+"\nexit 0\n")

	ref, err := resolveBaseImage(root, true)
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest, ref)
}

// buildx renders a field it cannot resolve as the empty string and still exits
// 0. Accepting that composes a --build-arg ending in a bare "@", which reads as
// a reference and resolves to nothing.
func TestImageDigest_RejectsOutputThatIsNotADigest(t *testing.T) {
	writeFakeDocker(t, "echo ''\nexit 0\n")

	_, err := imageDigest("ghcr.io/codefly-dev/codefly:0.0.4")
	require.ErrorContains(t, err, "not a digest")
}

func TestImageDigest_ReportsWhatDockerWroteToStderr(t *testing.T) {
	writeFakeDocker(t, `
echo "ghcr.io/codefly-dev/codefly:0.0.4: not found" 1>&2
exit 1
`)

	_, err := imageDigest("ghcr.io/codefly-dev/codefly:0.0.4")
	require.ErrorContains(t, err, "not found")
}

// The digest a dependent is pinned to has to be the one this run pushed. The
// base tag still points at its predecessor until the push lands, so reading it
// any earlier pins every dependent to the image the run is replacing.
func TestBuildTargets_PinsDependentsToTheDigestPushedByThisRun(t *testing.T) {
	root := t.TempDir()
	writeSiblingCLI(t, root)
	writeManifest(t, root, baseCompanionName, "0.0.5", true, false)
	writeManifest(t, root, "go", "0.0.9", true, false)
	writeBaseDependentDockerfile(t, root, "go", "ghcr.io/codefly-dev/codefly:0.0.4")

	dockerLog := filepath.Join(t.TempDir(), "docker.txt")
	writeFakeDocker(t, fmt.Sprintf(`
echo "$@" >> %[1]q
if [ "$1 $2" = "buildx imagetools" ]; then
  if grep -q "^push " %[1]q; then echo sha256:%[2]s; else echo sha256:%[3]s; fi
  exit 0
fi
if [ "$1" = "manifest" ]; then echo '{}'; fi
exit 0
`, dockerLog, pushedBaseDigest, predecessorBaseDigest))

	targets, err := selectTargets(root, true, nil)
	require.NoError(t, err)
	published, err := buildTargets(root, targets, BuildOptions{Push: true})
	require.NoError(t, err)
	require.Len(t, published, 2)

	logged, err := os.ReadFile(dockerLog)
	require.NoError(t, err)
	var dependentBuild string
	for _, line := range strings.Split(string(logged), "\n") {
		if strings.HasPrefix(line, "build ") && strings.Contains(line, "companions/go/Dockerfile") {
			dependentBuild = line
		}
	}
	require.NotEmpty(t, dependentBuild, "docker invocations: %s", logged)
	require.Contains(t, dependentBuild,
		"--build-arg "+baseImageArg+"=ghcr.io/codefly-dev/codefly@sha256:"+pushedBaseDigest)
	require.NotContains(t, dependentBuild, predecessorBaseDigest,
		"the base digest must be read after this run pushed the base, not before")
}

const (
	pushedBaseDigest      = "1111111111111111111111111111111111111111111111111111111111111111"
	predecessorBaseDigest = "2222222222222222222222222222222222222222222222222222222222222222"
)
