package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeFakeDocker installs a fake `docker` executable at the front of PATH
// for the duration of the test. body is the shell script run for every
// invocation, regardless of subcommand.
func writeFakeDocker(t *testing.T, body string) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\n" + body
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "docker"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// agentRepo writes a minimal agent repo: the manifest publish reads, plus a
// Dockerfile when the repo ships a runtime image.
func agentRepo(t *testing.T, kind, name string, dockerfile bool) string {
	t.Helper()
	dir := t.TempDir()
	manifest := fmt.Sprintf("publisher: codefly.dev\nkind: %s\nname: %s\nversion: 0.1.0\n", kind, name)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.codefly.yaml"), []byte(manifest), 0o600))
	if dockerfile {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0o600))
	}
	return dir
}

// TestAgentImage_ReferenceAddressesTheCanonicalRegistry pins the contract
// the whole change exists for: the reference is derived from
// resources.PublishedImage, so no registry hostname is spelled out in this
// repo or in any agent repo. The package name is the agent's GitHub
// repository, which is what a ghcr package is linked to.
func TestAgentImage_ReferenceAddressesTheCanonicalRegistry(t *testing.T) {
	for _, tc := range []struct {
		kind, name, want string
	}{
		{"codefly:service", "redis", resources.ImageRegistry + "/service-redis:0.0.86"},
		{"codefly:toolbox", "web", resources.ImageRegistry + "/toolbox-web:0.0.86"},
		{"codefly:provider", "stripe", resources.ImageRegistry + "/provider-stripe:0.0.86"},
		// A legacy manifest with no kind is a service.
		{"", "redis", resources.ImageRegistry + "/service-redis:0.0.86"},
	} {
		t.Run(tc.kind+"/"+tc.name, func(t *testing.T) {
			image, err := newAgentImage(agentRepo(t, tc.kind, tc.name, true), tc.kind, tc.name)
			require.NoError(t, err)
			require.NotNil(t, image)
			require.Equal(t, tc.want, image.reference("0.0.86"))
		})
	}
}

func TestNewAgentImage_NilWhenRepoShipsNoDockerfile(t *testing.T) {
	image, err := newAgentImage(agentRepo(t, "codefly:service", "mssql", false), "codefly:service", "mssql")
	require.NoError(t, err)
	require.Nil(t, image, "a repo with no Dockerfile publishes no runtime image")
}

// TestAgentImageBuildxArgs_ValidateProducesNoArtifact covers the split that
// makes the flow abortable: the pre-tag build must compile every published
// platform without pushing (a multi-platform result cannot be loaded
// locally, so it goes to the cache), and only the post-tag call pushes.
func TestAgentImageBuildxArgs_ValidateProducesNoArtifact(t *testing.T) {
	image := &agentImage{dir: "/repo", name: "service-redis"}

	validate := image.buildxArgs("0.0.86", false)
	require.Contains(t, validate, "--output")
	require.Contains(t, validate, "type=cacheonly")
	require.NotContains(t, validate, "--push")

	push := image.buildxArgs("0.0.86", true)
	require.Contains(t, push, "--push")
	require.NotContains(t, push, "type=cacheonly")

	for _, args := range [][]string{validate, push} {
		require.Equal(t, strings.Join(agentImagePlatforms, ","), args[argAfter(t, args, "--platform")],
			"every published image must carry both consumer platforms")
		require.Equal(t, resources.ImageRegistry+"/service-redis:0.0.86", args[argAfter(t, args, "-t")])
		require.Equal(t, "Dockerfile", args[argAfter(t, args, "-f")])
		require.Equal(t, ".", args[len(args)-1], "build context is the agent repo root")
	}
}

func TestAgentImage_BuildRunsInTheRepoRoot(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.txt")
	writeFakeDocker(t, fmt.Sprintf(`
{ echo "argv: $@"; echo "cwd: $(pwd -P)"; } >> %q
exit 0
`, record))

	dir := agentRepo(t, "codefly:service", "redis", true)
	image, err := newAgentImage(dir, "codefly:service", "redis")
	require.NoError(t, err)
	require.NoError(t, image.build(context.Background(), "0.0.86"))

	got := readRecord(t, record)
	require.Contains(t, got, "argv: buildx build --platform linux/amd64,linux/arm64 -f Dockerfile -t "+
		resources.ImageRegistry+"/service-redis:0.0.86 --output type=cacheonly .")
	// t.TempDir can hand back a symlinked path (/var → /private/var on
	// darwin), which is why the fake docker reports `pwd -P`.
	require.Contains(t, got, "cwd: "+evalSymlinks(t, dir))
}

func TestAgentImage_BuildFailureAbortsBeforeAnyGitMutation(t *testing.T) {
	writeFakeDocker(t, `
echo "ERROR: failed to solve: dockerfile parse error" 1>&2
exit 1
`)
	dir := agentRepo(t, "codefly:service", "redis", true)
	image, err := newAgentImage(dir, "codefly:service", "redis")
	require.NoError(t, err)

	err = image.build(context.Background(), "0.0.86")
	require.ErrorContains(t, err, "build runtime image "+resources.ImageRegistry+"/service-redis:0.0.86")
}

// TestAgentImage_PublishVerifiesAnonymously is the property the issue asks
// for: publishing runs the same credential-free manifest check `codefly
// companion verify` runs, so a push that only worked because the operator
// is logged in cannot ship a package nobody else can pull.
func TestAgentImage_PublishVerifiesAnonymously(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.txt")
	writeFakeDocker(t, fmt.Sprintf(`
case "$1" in
  manifest)
    { echo "argv: $@"; cat "$DOCKER_CONFIG/config.json"; } >> %q
    echo '{}'
    exit 0
    ;;
esac
echo "argv: $@" >> %q
exit 0
`, record, record))

	dir := agentRepo(t, "codefly:service", "redis", true)
	image, err := newAgentImage(dir, "codefly:service", "redis")
	require.NoError(t, err)
	require.NoError(t, image.publish(context.Background(), "0.0.86"))

	ref := resources.ImageRegistry + "/service-redis:0.0.86"
	got := readRecord(t, record)
	require.Contains(t, got, "--push")
	require.Contains(t, got, "argv: manifest inspect "+ref)
	require.Contains(t, got, "{}", "the manifest check must run with a credential-free docker config")
}

func TestAgentImage_PublishFailsWhenPushedButNotPublic(t *testing.T) {
	writeFakeDocker(t, `
case "$1" in
  manifest)
    echo "denied: requested access to the resource is denied" 1>&2
    exit 1
    ;;
esac
exit 0
`)
	dir := agentRepo(t, "codefly:service", "redis", true)
	image, err := newAgentImage(dir, "codefly:service", "redis")
	require.NoError(t, err)

	err = image.publish(context.Background(), "0.0.86")
	require.ErrorContains(t, err, "not publicly pullable")
	require.ErrorContains(t, err,
		"https://github.com/orgs/codefly-dev/packages/container/service-redis/settings")
}

func TestCheckAgentImagePreconditions_SkipsRepoWithoutDockerfile(t *testing.T) {
	writeFakeDocker(t, "exit 127\n")
	require.NoError(t, checkAgentImagePreconditions(agentRepo(t, "codefly:service", "mssql", false)),
		"a repo that ships no runtime image must not require docker")
}

func TestCheckAgentImagePreconditions_RejectsHostWithoutBuildx(t *testing.T) {
	writeFakeDocker(t, `
echo "unknown command: docker buildx" 1>&2
exit 1
`)
	err := checkAgentImagePreconditions(agentRepo(t, "codefly:service", "redis", true))
	require.ErrorContains(t, err, "docker buildx` is unavailable")
}

// TestAgentReleaseGate_PicksUpTheRepoDockerfile asserts the rule is the
// repo's Dockerfile, not the repo's kind: a source-tag kind (module,
// provider) that checks one in publishes its image too, and a repo without
// one is left alone.
func TestAgentReleaseGate_PicksUpTheRepoDockerfile(t *testing.T) {
	writeFakeDocker(t, "exit 0\n")

	for _, tc := range []struct {
		kind, name, wantImage string
	}{
		{"codefly:module", "saas-starter", resources.ImageRegistry + "/module-saas-starter:0.1.1"},
		{"codefly:provider", "stripe", resources.ImageRegistry + "/provider-stripe:0.1.1"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			gate, err := newAgentReleaseGate(agentRepo(t, tc.kind, tc.name, true))
			require.NoError(t, err)
			defer gate.cleanup()

			releaser, ok := gate.(*sourceTagReleaser)
			require.True(t, ok)
			require.NotNil(t, releaser.image)
			require.Equal(t, tc.wantImage, releaser.image.reference("0.1.1"))

			engine := &Engine{}
			releaser.attach(engine)
			require.NotNil(t, engine.AfterPush, "a repo with a Dockerfile must publish it after the tag lands")
		})
	}

	t.Run("no Dockerfile", func(t *testing.T) {
		gate, err := newAgentReleaseGate(agentRepo(t, "codefly:module", "saas-starter", false))
		require.NoError(t, err)
		defer gate.cleanup()

		releaser, ok := gate.(*sourceTagReleaser)
		require.True(t, ok)
		require.Nil(t, releaser.image)

		engine := &Engine{}
		releaser.attach(engine)
		require.Nil(t, engine.AfterPush, "a source-only repo must publish nothing beyond its tag")
	})
}

// --- helpers -------------------------------------------------------

// argAfter returns the index of the value following flag in args.
func argAfter(t *testing.T, args []string, flag string) int {
	t.Helper()
	for i, arg := range args {
		if arg == flag {
			require.Less(t, i+1, len(args), "%s has no value", flag)
			return i + 1
		}
	}
	t.Fatalf("%s not found in %v", flag, args)
	return 0
}

func readRecord(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	return string(raw)
}

func evalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return resolved
}
