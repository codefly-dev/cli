package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestBuildxArgsPushIsMultiArchManifestListOnContainerBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
		Target:    "final",
		BuildArgs: map[string]string{"VERSION": "1", "COMMIT": "abc"},
	}
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, true, "/tmp/meta.json", "")
	joined := strings.Join(args, " ")

	require.Equal(t, []string{"buildx", "build"}, args[:2])
	// Multi-platform builds must run on the container-driver builder.
	require.Contains(t, joined, "--builder codefly")
	require.Contains(t, joined, "--platform linux/amd64,linux/arm64")
	require.Contains(t, joined, "--push")
	require.NotContains(t, joined, "--load")
	require.Contains(t, joined, "--metadata-file /tmp/meta.json")
	require.Contains(t, joined, "--target final")
	// Build args are emitted in sorted key order for a stable command.
	require.Contains(t, joined, "--build-arg COMMIT=abc --build-arg VERSION=1")
	require.Equal(t, []string{"-t", "repo/app:v1", "-f", "/svc/builder/Dockerfile", "/svc"}, args[len(args)-5:])
}

func TestBuildxArgsLocalBuildIsSinglePlatformLoadOnDefaultBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", false, false, "", "")
	joined := strings.Join(args, " ")

	// A local load cannot materialize a multi-platform manifest list, and it
	// uses the default builder (no dedicated container builder needed).
	require.NotContains(t, joined, "--builder")
	require.Contains(t, joined, "--platform linux/amd64")
	require.NotContains(t, joined, "linux/amd64,linux/arm64")
	require.Contains(t, joined, "--load")
	require.NotContains(t, joined, "--push")
	require.NotContains(t, joined, "--metadata-file")
}

func TestBuildxArgsCallerBuilderWinsOverContainerBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64", "linux/arm64"},
	}
	// A caller-provided builder (e.g. a native amd64 buildkit) is authoritative,
	// even for a multi-arch push that would otherwise select "codefly".
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, true, "/tmp/meta.json", "amd64-remote")
	joined := strings.Join(args, " ")

	require.Contains(t, joined, "--builder amd64-remote")
	require.NotContains(t, joined, "--builder codefly")
	require.Contains(t, joined, "--push")
}

func TestBuildxArgsSinglePlatformPushOnCallerBuilder(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{
		Image:     "repo/app:v1",
		Platforms: []string{"linux/amd64"},
	}
	// The #501 targeted amd64 fix: one platform, pushed on a native builder so
	// no local QEMU is involved. Single-platform builds are not "multiArch".
	args := buildxArgs(recipe, "/svc/builder/Dockerfile", "/svc", true, false, "/tmp/meta.json", "amd64-remote")
	joined := strings.Join(args, " ")

	require.Contains(t, joined, "--builder amd64-remote")
	require.Contains(t, joined, "--platform linux/amd64")
	require.Contains(t, joined, "--push")
	require.NotContains(t, joined, "--load")
}

func TestBuildxCreateArgsUsesHostNetworkContainerDriver(t *testing.T) {
	args := buildxCreateArgs()
	joined := strings.Join(args, " ")

	require.Equal(t, []string{"buildx", "create"}, args[:2])
	require.Contains(t, joined, "--name codefly")
	require.Contains(t, joined, "--driver docker-container")
	// Host networking lets BuildKit inherit the host resolver so it can reach a
	// tailnet split-DNS private registry instead of stalling on DNS.
	require.Contains(t, joined, "--driver-opt network=host")
	require.Contains(t, joined, "--bootstrap")
}

func TestBuilderHasHostNetwork(t *testing.T) {
	// A builder created with --driver-opt network=host reports it under Driver
	// Options; a pre-fix builder omits the line entirely and must read as stale
	// so the caller refuses to reuse it instead of stalling on tailnet DNS.
	ready := []byte("Name:          codefly\nDriver:        docker-container\nNodes:\nName:                  codefly0\nDriver Options:        network=\"host\"\nStatus:                running\n")
	require.True(t, builderHasHostNetwork(ready))

	stale := []byte("Name:          codefly\nDriver:        docker-container\nNodes:\nName:                  codefly0\nStatus:                running\n")
	require.False(t, builderHasHostNetwork(stale))
}

func TestPlatformsIncludeDeploymentArch(t *testing.T) {
	require.True(t, platformsIncludeDeploymentArch([]string{"linux/arm64", "linux/amd64"}))
	require.True(t, platformsIncludeDeploymentArch([]string{"linux/amd64/v2"}))
	// An arm64-only recipe would deploy an image that cannot run on amd64 nodes.
	require.False(t, platformsIncludeDeploymentArch([]string{"linux/arm64"}))
	// An empty list builds only the host arch — the arm64-on-Apple-silicon bug.
	require.False(t, platformsIncludeDeploymentArch(nil))
}

func TestRecipeDockerfileResolvesAndContains(t *testing.T) {
	outputDir := filepath.FromSlash("/work/services/store/builder")

	got, err := recipeDockerfile(outputDir, &builderv0.DockerBuildRecipe{Dockerfile: "Dockerfile"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(outputDir, "Dockerfile"), got)

	// A recipe must not point buildx -f at a file outside the recipe tree.
	_, err = recipeDockerfile(outputDir, &builderv0.DockerBuildRecipe{Dockerfile: "../../../../etc/passwd"})
	require.Error(t, err)
}

func TestRecipeContextResolvesAndContains(t *testing.T) {
	serviceDir := "/work/services/store"

	got, err := recipeContext(serviceDir, &builderv0.DockerBuildRecipe{Context: ""})
	require.NoError(t, err)
	require.Equal(t, serviceDir, got)

	got, err = recipeContext(serviceDir, &builderv0.DockerBuildRecipe{Context: "code"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(serviceDir, "code"), got)

	_, err = recipeContext(serviceDir, &builderv0.DockerBuildRecipe{Context: "../other"})
	require.Error(t, err)
}

func TestReadPushedImageDigest(t *testing.T) {
	metadata := filepath.Join(t.TempDir(), "meta.json")
	digest := "sha256:" + strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(metadata, []byte(`{"containerimage.digest":"`+digest+`","image.name":"repo/app:v1"}`), 0o644))
	got, err := readPushedImageDigest(metadata)
	require.NoError(t, err)
	require.Equal(t, digest, got)

	bad := filepath.Join(t.TempDir(), "meta.json")
	require.NoError(t, os.WriteFile(bad, []byte(`{"image.name":"repo/app:v1"}`), 0o644))
	_, err = readPushedImageDigest(bad)
	require.Error(t, err)
}

func TestBuildRecipeOutputDirectoryIsAbsoluteAndDoesNotCreate(t *testing.T) {
	serviceDir := t.TempDir()
	outputDir, err := buildRecipeOutputDirectory(serviceDir)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(outputDir))
	require.Equal(t, filepath.Join(serviceDir, buildRecipeDir), outputDir)
	// The CLI must not create builder/ — a legacy agent that ignores the field
	// must not have an empty directory left behind.
	_, statErr := os.Stat(outputDir)
	require.True(t, os.IsNotExist(statErr))
}

func TestBuildCacheSeparatesServicesRecipesAndWorkspaces(t *testing.T) {
	cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "protected/app", Imports: []string{"ghcr.io/org/cache"}, Exports: []string{"ghcr.io/org/cache"}, Mode: "max"}
	references := map[string]bool{}
	for _, identity := range [][3]string{
		{"workspace", "app/api", "app"}, {"workspace", "app/worker", "app"},
		{"workspace", "app/api", "migration"}, {"workspace", "app/api", ""},
		{"other", "app/api", "app"}, {"workspace/app", "api", "app"},
	} {
		scoped := scopedBuildCache(cache, identity[0], identity[1], identity[2])
		require.Equal(t, cache.Imports, scoped.Imports)
		require.Equal(t, cache.Exports, scoped.Exports)
		require.Equal(t, cache.Mode, scoped.Mode)
		recipe := &builderv0.DockerBuildRecipe{Image: "ghcr.io/org/app:v1", Platforms: []string{"linux/amd64"}}
		args, err := cachedBuildxArgs(recipe, "Dockerfile", ".", false, false, "", "", scoped, privateModuleBuild{})
		require.NoError(t, err)
		export := args[slices.Index(args, "--cache-to")+1]
		require.False(t, references[export], "cache export collides for %v", identity)
		references[export] = true
		recipe.Image = "ghcr.io/org/app:v2"
		again, err := cachedBuildxArgs(recipe, "Dockerfile", ".", false, false, "", "", scopedBuildCache(cache, identity[0], identity[1], identity[2]), privateModuleBuild{})
		require.NoError(t, err)
		require.Equal(t, export, again[slices.Index(again, "--cache-to")+1])
	}
	require.Equal(t, "protected/app", cache.Scope)
	require.Nil(t, scopedBuildCache(nil, "workspace", "app/api", "app"))
}

// treeDigest lists every regular file under root with its content digest, so a
// before/after comparison proves a tree was left byte-identical.
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	digest := map[string]string{}
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			digest[relative+"/"] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		digest[relative] = hex.EncodeToString(sum[:])
		return nil
	}))
	return digest
}

func TestBuildRecipeRootKeepsAuthoredServicesAndRedirectsMaterializedOnes(t *testing.T) {
	workspace := t.TempDir()
	scratch := filepath.Join(workspace, ".codefly", "build", "saas", "store")
	for name, test := range map[string]struct {
		serviceDir string
		want       string
	}{
		"authored in the workspace": {
			serviceDir: filepath.Join(workspace, "modules", "saas", "services", "store"),
			want:       filepath.Join(workspace, "modules", "saas", "services", "store"),
		},
		"verified package under the workspace cache": {
			serviceDir: filepath.Join(workspace, ".codefly", "cache", "modules", "sha256-abc", "services", "store"),
			want:       scratch,
		},
		"git clone under a module cache root outside the workspace": {
			serviceDir: filepath.Join(t.TempDir(), "github.com", "example", "saas", "v1.2.3", "services", "store"),
			want:       scratch,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := buildRecipeRoot(workspace, "saas", "store", test.serviceDir)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
			output, err := buildRecipeOutputDirectory(got)
			require.NoError(t, err)
			require.Equal(t, filepath.Join(test.want, buildRecipeDir), output)
		})
	}

	// No workspace directory: nothing to classify against, the output stays
	// beside the service (the shape every Builder unit test constructs).
	serviceDir := filepath.Join(t.TempDir(), "services", "store")
	got, err := buildRecipeRoot("", "saas", "store", serviceDir)
	require.NoError(t, err)
	require.Equal(t, serviceDir, got)

	// A redirect needs the module/service identity to name its scratch slot.
	_, err = buildRecipeRoot(workspace, "", "store", filepath.Join(t.TempDir(), "store"))
	require.ErrorContains(t, err, "no module/service identity")
}

// materializedService lays out a service the way a verified module package is
// materialized under the workspace cache: the module tracks builder/Dockerfile,
// so a render that rewrote it would move the package's tree digest.
func materializedService(t *testing.T, workspace string) (*resources.Service, string) {
	t.Helper()
	serviceDir := filepath.Join(workspace, ".codefly", "cache", "modules", "sha256-abc", "services", "store")
	for name, content := range map[string]string{
		"service.codefly.yaml":       "name: store\n",
		"builder/Dockerfile":         "FROM scratch\nCOPY builder/runtime-access.sql /app/\n",
		"builder/runtime-access.sql": "GRANT SELECT ON ALL TABLES;\n",
		"code/main.go":               "package main\n",
	} {
		path := filepath.Join(serviceDir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	service := &resources.Service{
		Name:    "store",
		Version: "1.0.0",
		Agent:   &resources.Agent{Publisher: "codefly.dev", Name: "postgres", Version: "0.3.5"},
	}
	service.WithDir(serviceDir)
	return service, serviceDir
}

func TestBuildOfMaterializedServiceLeavesItsTreeByteIdentical(t *testing.T) {
	// The agent emits its recipe into the OutputDirectory the CLI hands it. For
	// a service under the workspace's module cache that directory must be the
	// .codefly/build scratch, never the service tree: the recipe lands, the CLI
	// verifies it there, and the materialized tree is byte-identical afterwards.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, &recipeProtocolPeer{})
	conn, err := grpc.NewClient(serveGRPC(t, server), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	observed := &observedRecipeClient{BuilderClient: builderv0.NewBuilderClient(conn)}

	workspaceDir := t.TempDir()
	workspace := &resources.Workspace{Name: "platform"}
	workspace.WithDir(workspaceDir)
	service, serviceDir := materializedService(t, workspaceDir)
	before := treeDigest(t, serviceDir)

	instance := &services.Instance{Service: service, Identity: &resources.ServiceIdentity{Name: "store", Module: "saas"}}
	instance.Builder = &services.BuilderInstance{Instance: instance, Builder: &coreservices.BuilderAgent{BuilderClient: observed}}
	build, err := NewBuilder(ctx, instance, &World{Workspace: workspace})
	require.NoError(t, err)

	// docker is absent on PATH: the agent has already emitted its recipe and the
	// CLI has verified it by the time the build is attempted.
	t.Setenv("PATH", t.TempDir())
	_, err = build.Build(ctx)
	require.ErrorIs(t, err, exec.ErrNotFound)

	scratch := filepath.Join(workspaceDir, ".codefly", "build", "saas", "store")
	require.NotNil(t, observed.request)
	require.Equal(t, filepath.Join(scratch, buildRecipeDir), observed.request.GetOutputDirectory())
	require.FileExists(t, filepath.Join(scratch, buildRecipeDir, "Dockerfile"))
	require.NoError(t, coreservices.VerifyDockerBuildPlan(observed.request.GetOutputDirectory(), observed.response.GetResult().GetDockerBuildPlan()))
	require.Equal(t, before, treeDigest(t, serviceDir), "a build must not write into a materialized service tree")
	// The tracked recipe the module ships is untouched, not overwritten by the
	// agent's fresh emission.
	tracked, err := os.ReadFile(filepath.Join(serviceDir, "builder", "Dockerfile"))
	require.NoError(t, err)
	require.Equal(t, "FROM scratch\nCOPY builder/runtime-access.sql /app/\n", string(tracked))
}

func TestRecordBuildRecipeOfMaterializedServiceArchivesUnderWorkspaceScratch(t *testing.T) {
	workspaceDir := t.TempDir()
	service, serviceDir := materializedService(t, workspaceDir)
	before := treeDigest(t, serviceDir)

	root, err := buildRecipeRoot(workspaceDir, "saas", "store", serviceDir)
	require.NoError(t, err)
	// The agent emitted a fresh recipe into the scratch builder/ (as
	// TestBuildOfMaterializedServiceLeavesItsTreeByteIdentical proves it is asked to).
	emitted := filepath.Join(root, buildRecipeSourceDir)
	require.NoError(t, os.MkdirAll(emitted, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(emitted, "Dockerfile"), []byte("FROM scratch\nLABEL fresh=true\n"), 0o644))

	plan := &builderv0.DockerBuildPlan{Recipes: []*builderv0.DockerBuildRecipe{{Name: "store", Image: "example.test/store:v1", Dockerfile: "Dockerfile", Context: "."}}}
	require.NoError(t, recordBuildRecipe(context.Background(), service, root, plan))

	archive := filepath.Join(workspaceDir, ".codefly", "build", "saas", "store", buildRecipeArchiveDir, "0.3.5")
	require.FileExists(t, filepath.Join(archive, "Dockerfile"))
	require.FileExists(t, filepath.Join(archive, buildRecipeManifest))
	require.NoDirExists(t, filepath.Join(serviceDir, buildRecipeArchiveDir))
	require.Equal(t, before, treeDigest(t, serviceDir), "recording the archive must not write into a materialized service tree")
}
