package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dockerStub stands in for the container engine at the only boundary an image
// build crosses. It counts `buildx build` invocations — the whole point of the
// cache is that the second one does not happen — and hands each build a fresh
// manifest digest, so a digest that did not move proves an image was kept rather
// than rebuilt to the same bytes by accident.
type dockerStub struct {
	dir string
}

func installDockerStub(t *testing.T, registryServes bool) *dockerStub {
	t.Helper()
	dir := t.TempDir()
	missing := ""
	if !registryServes {
		missing = "exit 1"
	}
	script := `#!/bin/sh
set -e
STATE="` + dir + `"
record() { printf '%s\n' "$1" >> "$STATE/calls"; }
case "$1 $2" in
"buildx build")
  record build
  count=0
  if [ -f "$STATE/count" ]; then read -r count < "$STATE/count"; fi
  count=$((count+1))
  printf '%s\n' "$count" > "$STATE/count"
  metadata=""
  previous=""
  for argument in "$@"; do
    if [ "$previous" = "--metadata-file" ]; then metadata="$argument"; fi
    previous="$argument"
  done
  if [ -n "$metadata" ]; then
    printf '{"containerimage.digest":"sha256:%064d"}' "$count" > "$metadata"
  fi
  exit 0
  ;;
"buildx imagetools")
  record imagetools
  ` + missing + `
  exit 0
  ;;
"image inspect")
  record inspect
  printf 'sha256:%064d\n' 7
  exit 0
  ;;
esac
record "unexpected $*"
exit 1
`
	path := filepath.Join(dir, "docker")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &dockerStub{dir: dir}
}

func (stub *dockerStub) calls(t *testing.T, kind string) int {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(stub.dir, "calls"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(payload)), "\n") {
		if line == kind {
			count++
		}
		require.NotContains(t, line, "unexpected", "the build made a container-engine call the stub does not model")
	}
	return count
}

// imageCachePeer is the agent side of the build: it emits the same recipe tree
// on every call, so a key that moves between two builds moved because something
// in the workspace moved, not because the agent re-rendered differently.
type imageCachePeer struct {
	builderv0.UnimplementedBuilderServer
}

func (*imageCachePeer) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{BuildxSelection: true}, nil
}

func (*imageCachePeer) Build(_ context.Context, request *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	output := request.GetOutputDirectory()
	if err := os.MkdirAll(output, 0o755); err != nil {
		return nil, err
	}
	for name, content := range map[string]string{
		"Dockerfile":   "FROM scratch\nCOPY code /app\n",
		"dockerignore": "notes.md\n",
	} {
		if err := os.WriteFile(filepath.Join(output, name), []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	plan, err := coreservices.SingleImageBuildPlan(output, "example.test/app:v1", coreservices.RecipeBuildPlatforms(), []string{"Dockerfile", "dockerignore"})
	if err != nil {
		return nil, err
	}
	return &builderv0.BuildResponse{
		State:  &builderv0.BuildStatus{State: builderv0.BuildStatus_SUCCESS},
		Result: &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildPlan{DockerBuildPlan: plan}},
	}, nil
}

// imageCacheHarness is one workspace holding one service, built repeatedly
// through the real Builder against the docker stub.
type imageCacheHarness struct {
	docker       *dockerStub
	workspaceDir string
	serviceDir   string
	client       *coreservices.BuilderAgent
	push         bool
	rebuild      bool
}

func newImageCacheHarness(t *testing.T, push, registryServes bool) *imageCacheHarness {
	t.Helper()
	stub := installDockerStub(t, registryServes)
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, &imageCachePeer{})
	conn, err := grpc.NewClient(serveGRPC(t, server), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	workspaceDir := t.TempDir()
	serviceDir := filepath.Join(workspaceDir, "modules", "saas", "services", "api")
	harness := &imageCacheHarness{
		docker:       stub,
		workspaceDir: workspaceDir,
		serviceDir:   serviceDir,
		client:       &coreservices.BuilderAgent{BuilderClient: builderv0.NewBuilderClient(conn)},
		push:         push,
	}
	harness.write(t, filepath.Join(serviceDir, "service.codefly.yaml"), "name: api\n")
	harness.write(t, filepath.Join(serviceDir, "code", "main.go"), "package main\n\nfunc main() {}\n")
	harness.write(t, filepath.Join(serviceDir, "code", "go.mod"), "module example.test/api\n\ngo 1.25\n")
	harness.write(t, filepath.Join(serviceDir, "code", "go.sum"), "")
	harness.write(t, filepath.Join(serviceDir, "notes.md"), "scratch\n")
	harness.write(t, filepath.Join(workspaceDir, "configurations", "local.codefly.yaml"), "name: local\nvalue: one\n")
	return harness
}

func (harness *imageCacheHarness) write(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func (harness *imageCacheHarness) read(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(payload)
}

// build runs one full Builder.Build and returns the image identity it resolved:
// the pushed manifest digest, or the loaded image id for a build that does not
// push.
func (harness *imageCacheHarness) build(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	workspace := &resources.Workspace{Name: "platform"}
	workspace.WithDir(harness.workspaceDir)
	service := &resources.Service{
		Name:    "api",
		Version: "1.0.0",
		Agent:   &resources.Agent{Publisher: "codefly.dev", Name: "go", Version: "0.1.0"},
	}
	service.WithDir(harness.serviceDir)
	instance := &services.Instance{Service: service, Identity: &resources.ServiceIdentity{Name: "api", Module: "saas"}}
	instance.Builder = &services.BuilderInstance{Instance: instance, Builder: harness.client}
	world := &World{
		Workspace:          workspace,
		Push:               harness.push,
		CaptureImageDigest: harness.push,
		BuildxBuilder:      "stub",
		RebuildImages:      harness.rebuild,
	}
	build, err := NewBuilder(ctx, instance, world)
	require.NoError(t, err)
	_, err = build.Build(ctx)
	require.NoError(t, err)
	if harness.push {
		require.NotEmpty(t, build.ImageDigest())
		return build.ImageDigest()
	}
	entry, found := build.imageBuildCache().lookup(harness.key(t))
	require.True(t, found, "a local build records the image it loaded")
	return entry.ImageID
}

// key is the single entry the harness's one recipe records.
func (harness *imageCacheHarness) key(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(harness.workspaceDir, filepath.FromSlash(imageCacheDir)))
	require.NoError(t, err)
	require.Len(t, entries, 1, "one recipe records one entry")
	return "sha256:" + strings.TrimSuffix(entries[0].Name(), ".json")
}

// A configuration-only change is the case this exists for: the value the service
// reads moves, nothing the image is built from does, and the image keeps its
// digest instead of being rebuilt. The second build also proves the recipe
// archive the first one recorded beside the service does not invalidate its own
// identity — that would rebuild on every second run, forever.
func TestImageIsKeptWhenNoImageInputChanged(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	first := harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	require.DirExists(t, filepath.Join(harness.serviceDir, buildRecipeArchiveDir, "0.1.0"))

	harness.write(t, filepath.Join(harness.workspaceDir, "configurations", "local.codefly.yaml"), "name: local\nvalue: two\n")
	require.Equal(t, first, harness.build(t))
	require.Equal(t, 1, harness.docker.calls(t, "build"), "a configuration edit must not reach the image build")

	// The same holds for a file inside the build context that the recipe's own
	// ignore policy excludes: Docker never sends it, so it is not an input.
	harness.write(t, filepath.Join(harness.serviceDir, "notes.md"), "rewritten\n")
	require.Equal(t, first, harness.build(t))
	require.Equal(t, 1, harness.docker.calls(t, "build"))
}

// The regression this must never create. A source edit in a language whose
// dependency manifest did not move is the exact shape that a cache keyed on a
// dependency hash serves stale, and the failure presents as the code not
// working rather than as the build being old. The manifests are read back to
// show the test really did leave them alone.
func TestSourceEditAlwaysProducesANewImageEvenWithNoManifestChange(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	manifest := filepath.Join(harness.serviceDir, "code", "go.mod")
	sum := filepath.Join(harness.serviceDir, "code", "go.sum")
	before, beforeSum := harness.read(t, manifest), harness.read(t, sum)

	first := harness.build(t)
	harness.write(t, filepath.Join(harness.serviceDir, "code", "main.go"), "package main\n\nfunc main() { println(\"fixed\") }\n")
	second := harness.build(t)

	require.Equal(t, before, harness.read(t, manifest), "the dependency manifest must not have moved")
	require.Equal(t, beforeSum, harness.read(t, sum), "the dependency lock must not have moved")
	require.NotEqual(t, first, second, "a source edit must produce a new image")
	require.Equal(t, 2, harness.docker.calls(t, "build"))
}

// Every shape of change inside the build context moves the identity, not only an
// edit to a file that already existed.
func TestEveryContextChangeMovesTheImageIdentity(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *imageCacheHarness){
		"file added": func(t *testing.T, harness *imageCacheHarness) {
			harness.write(t, filepath.Join(harness.serviceDir, "code", "extra.go"), "package main\n")
		},
		"file removed": func(t *testing.T, harness *imageCacheHarness) {
			require.NoError(t, os.Remove(filepath.Join(harness.serviceDir, "code", "main.go")))
		},
		"file renamed": func(t *testing.T, harness *imageCacheHarness) {
			code := filepath.Join(harness.serviceDir, "code")
			require.NoError(t, os.Rename(filepath.Join(code, "main.go"), filepath.Join(code, "entry.go")))
		},
		"mode changed": func(t *testing.T, harness *imageCacheHarness) {
			require.NoError(t, os.Chmod(filepath.Join(harness.serviceDir, "code", "main.go"), 0o755))
		},
		"file replaced by a symlink": func(t *testing.T, harness *imageCacheHarness) {
			path := filepath.Join(harness.serviceDir, "code", "main.go")
			require.NoError(t, os.Remove(path))
			require.NoError(t, os.Symlink("elsewhere.go", path))
		},
		"dependency manifest changed": func(t *testing.T, harness *imageCacheHarness) {
			harness.write(t, filepath.Join(harness.serviceDir, "code", "go.mod"), "module example.test/api\n\ngo 1.25\n\nrequire example.test/dep v1.0.0\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			harness := newImageCacheHarness(t, true, true)
			first := harness.build(t)
			change(t, harness)
			require.NotEqual(t, first, harness.build(t))
			require.Equal(t, 2, harness.docker.calls(t, "build"))
		})
	}
}

// A build that never pushes has no registry manifest, so its entry is bound to
// the image the daemon holds and is verified against it.
func TestLocalBuildKeepsTheLoadedImageWhenNoInputChanged(t *testing.T) {
	harness := newImageCacheHarness(t, false, true)
	first := harness.build(t)
	require.Equal(t, 1, harness.docker.calls(t, "build"))
	harness.write(t, filepath.Join(harness.workspaceDir, "configurations", "local.codefly.yaml"), "name: local\nvalue: two\n")
	require.Equal(t, first, harness.build(t))
	require.Equal(t, 1, harness.docker.calls(t, "build"))
}

// The escape hatch has to work without an input changing, because it exists for
// the case where someone suspects the cache rather than the code.
func TestRebuildBuildsEvenWhenNoInputChanged(t *testing.T) {
	harness := newImageCacheHarness(t, true, true)
	first := harness.build(t)
	harness.rebuild = true
	require.NotEqual(t, first, harness.build(t))
	require.Equal(t, 2, harness.docker.calls(t, "build"))
}

// An entry is a claim about an image, not the image. A registry that no longer
// serves the recorded manifest must produce a build, not a reference to nothing.
func TestAVanishedImageIsRebuiltRatherThanReused(t *testing.T) {
	harness := newImageCacheHarness(t, true, false)
	first := harness.build(t)
	second := harness.build(t)
	require.NotEqual(t, first, second)
	require.Equal(t, 2, harness.docker.calls(t, "build"))
}

func TestNormalizedBuildxArgsBindsTheBuildAndNotWhereItRan(t *testing.T) {
	proxies := map[string]string{"proxy": "/tmp/codefly-go-modules-123/modcache/cache/download"}
	args := []string{
		"buildx", "build", "--builder", "remote-amd64", "--platform", "linux/amd64", "--push",
		"--metadata-file", "/tmp/codefly-build-metadata-9.json", "--build-arg", "VERSION=1",
		"--build-context", "proxy=" + proxies["proxy"],
		"-t", "repo/app:v1", "-f", "/ws/services/api/builder/Dockerfile", "/ws/services/api",
	}
	normalized := normalizedBuildxArgs(args, "/ws/services/api", "/ws/services/api/builder/Dockerfile", proxies)

	joined := strings.Join(normalized, " ")
	require.NotContains(t, joined, "remote-amd64", "where a build runs does not change the image it produces")
	require.NotContains(t, joined, "codefly-build-metadata", "where buildx writes its metadata is not an input")
	require.NotContains(t, joined, "codefly-go-modules", "the prefetch's per-run directory is not an input")
	require.NotContains(t, joined, "/ws/services/api", "the context's absolute location is not an input")
	// What the build was asked to produce stays, verbatim.
	require.Contains(t, joined, "--platform linux/amd64")
	require.Contains(t, joined, "--push")
	require.Contains(t, joined, "--build-arg VERSION=1")
	require.Contains(t, joined, "--build-context proxy={proxy}")
	require.Contains(t, joined, "-t repo/app:v1")
	require.Contains(t, joined, "-f {dockerfile}")
}

func TestDigestReferenceReplacesTheTagWithTheRecordedManifest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for image, want := range map[string]string{
		"example.test/app:v1":             "example.test/app@" + digest,
		"registry.local:5000/team/app:v1": "registry.local:5000/team/app@" + digest,
		"registry.local:5000/team/app":    "registry.local:5000/team/app@" + digest,
		"app":                             "app@" + digest,
	} {
		reference, ok := digestReference(image, digest)
		require.True(t, ok, image)
		require.Equal(t, want, reference)
	}
	_, ok := digestReference("example.test/app:v1", "not-a-digest")
	require.False(t, ok, "an entry with no sha256 manifest cannot be verified")
}
