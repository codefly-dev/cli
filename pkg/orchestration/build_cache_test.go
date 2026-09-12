package orchestration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/agents/manager"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func TestBuildCacheRetainsCallerPolicyAndNegotiatesExecution(t *testing.T) {
	for _, recipe := range []bool{false, true} {
		t.Run(fmt.Sprintf("recipe=%t", recipe), func(t *testing.T) {
			ctx := context.Background()
			base := &coreservices.Base{}
			require.NoError(t, base.HeadlessLoad(ctx, &basev0.ServiceIdentity{Name: "api", Module: "app", WorkspacePath: t.TempDir()}))
			requests := make(chan *builderv0.BuildRequest, 2)
			server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				if request, ok := req.(*builderv0.BuildRequest); ok {
					requests <- proto.CloneOf(request)
				}
				return handler(ctx, req)
			}))
			wrapper := &coreservices.BuilderWrapper{Base: base}
			if recipe {
				wrapper.BuildResult = &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildPlan{DockerBuildPlan: &builderv0.DockerBuildPlan{}}}
			}
			builderv0.RegisterBuilderServer(server, coreservices.NewDefaultBuilder(wrapper))
			address := serveGRPC(t, server)
			conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			client := coreservices.NewBuilderAgentClient(conn)
			response, err := client.Build(ctx, &builderv0.BuildRequest{})
			require.NoError(t, err)
			require.Equal(t, builderv0.BuildStatus_SUCCESS, response.GetState().GetState())
			<-requests

			cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "app/api", Imports: []string{"ghcr.io/org/cache"}}
			flow := &Flow{world: &World{Workspace: &resources.Workspace{}}}
			flow.WithBuildCache(cache)
			flow.WithBuildxBuilder("selected")
			cache.Imports[0] = "ghcr.io/org/changed"
			require.Equal(t, "ghcr.io/org/cache", flow.world.BuildCache.Imports[0])
			require.Nil(t, (&World{}).BuildCache)
			service := serviceWithRecipe(t, "1.0.0", nil)
			instance := &services.Instance{Service: service, Identity: &resources.ServiceIdentity{Name: "api", Module: "app"}}
			instance.Builder = &services.BuilderInstance{Instance: instance, Builder: client}
			build, err := NewBuilder(ctx, instance, flow.world)
			require.NoError(t, err)
			dockerContext, err := build.dockerBuildContext(ctx)
			require.NoError(t, err)
			require.Equal(t, "selected", dockerContext.GetBuildxBuilder())
			_, err = build.Build(ctx)
			require.ErrorContains(t, err, "cannot verify builder agent support for requested Buildx builder")
			require.Empty(t, requests, "unsupported agents must be rejected before the Build RPC")

			forwarded := dockerContext.GetCache()
			require.Equal(t, `["app/api","","app/api",""]`, forwarded.Scope)
			require.Equal(t, flow.world.BuildCache.Imports, forwarded.Imports)
			require.Equal(t, flow.world.BuildCache.Exports, forwarded.Exports)
			require.True(t, proto.Equal(flow.world.BuildCache, &builderv0.BuildCacheOptions{Backend: "registry", Scope: "app/api", Imports: []string{"ghcr.io/org/cache"}}))
			flow.WithBuildCache(nil)
			require.Nil(t, flow.world.BuildCache)
		})
	}
}

func TestBuildCacheRequiresCallerScopeBeforeAddingServiceIdentity(t *testing.T) {
	builder := &Builder{
		world:    &World{Workspace: &resources.Workspace{Name: "workspace"}, BuildxBuilder: "selected", BuildCache: &builderv0.BuildCacheOptions{Backend: "registry", Exports: []string{"ghcr.io/org/cache"}}},
		instance: &services.Instance{Identity: &resources.ServiceIdentity{Name: "api", Module: "app"}},
	}
	_, err := builder.dockerBuildContext(t.Context())
	require.ErrorContains(t, err, "build cache scope is required")
}

func TestBuildCacheIsOwnedByFlowAndScopedByServiceAndRecipe(t *testing.T) {
	cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "workspace/protected", Exports: []string{"ghcr.io/org/cache"}}
	flow := &Flow{world: &World{}}
	flow.WithBuildCache(cache)
	cache.Exports[0] = "ghcr.io/different/cache"
	require.Equal(t, "ghcr.io/org/cache", flow.world.BuildCache.Exports[0])
	a := scopedBuildCache(flow.world.BuildCache, "module/a", "app")
	b := scopedBuildCache(flow.world.BuildCache, "module/b", "app")
	migration := scopedBuildCache(flow.world.BuildCache, "module/a", "migration")
	require.NotEqual(t, a.Scope, b.Scope)
	require.NotEqual(t, a.Scope, migration.Scope)
	require.Equal(t, "workspace/protected", flow.world.BuildCache.Scope)
}

func TestCachedBuildxArgsMatchExecutedPlatformsAndPreserveOutputIdentity(t *testing.T) {
	recipe := &builderv0.DockerBuildRecipe{Name: "app", Image: "ghcr.io/org/app:v1", Platforms: []string{"linux/amd64", "linux/arm64"}, Target: "runtime", BuildArgs: map[string]string{"PUBLIC": "value"}}
	cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "workspace/service/app", Imports: []string{"ghcr.io/org/cache"}, Exports: []string{"ghcr.io/org/cache"}}
	for _, push := range []bool{false, true} {
		args, err := cachedBuildxArgs(recipe, "/staged/Dockerfile", "/staged/context", push, push, "/tmp/metadata", "remote", cache)
		require.NoError(t, err)
		require.Equal(t, "/staged/context", args[len(args)-1])
		require.Contains(t, args, "--metadata-file")
		require.Contains(t, args, "/tmp/metadata")
		require.Contains(t, args, "runtime")
		require.Contains(t, args, "PUBLIC=value")
		require.Contains(t, args, recipe.Image)
		require.Contains(t, args, "--pull")
		require.Contains(t, args, "--progress")
		require.Contains(t, args, "remote")
		count := 0
		for _, arg := range args {
			if arg == "--cache-to" {
				count++
			}
		}
		if push {
			require.Equal(t, 2, count)
			require.Contains(t, args, "linux/amd64,linux/arm64")
			require.Contains(t, args, "--push")
		} else {
			require.Equal(t, 1, count)
			require.Contains(t, args, "linux/amd64")
			require.Contains(t, args, "--load")
		}
	}
	cache.Exports = nil
	args, err := cachedBuildxArgs(recipe, "/staged/Dockerfile", "/staged/context", false, false, "", "", cache)
	require.NoError(t, err)
	require.NotContains(t, args, "--cache-to")
	cache.Backend = "unsupported"
	_, err = cachedBuildxArgs(recipe, "/staged/Dockerfile", "/staged/context", false, false, "", "", cache)
	require.ErrorContains(t, err, "unsupported")
}

type observedRecipeClient struct {
	builderv0.BuilderClient
	request  *builderv0.BuildRequest
	response *builderv0.BuildResponse
}

func (client *observedRecipeClient) Build(ctx context.Context, request *builderv0.BuildRequest, options ...grpc.CallOption) (*builderv0.BuildResponse, error) {
	client.request = proto.CloneOf(request)
	response, err := client.BuilderClient.Build(ctx, request, options...)
	client.response = response
	return response, err
}

func TestRecipeAgentsReceiveBuildContextAndOutputDirectory(t *testing.T) {
	if os.Getenv(resources.CodeflyHomeEnv) == "" {
		t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	}
	for _, name := range []string{"go", "nextjs"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			plugin, ok := sourceworkspace.PinnedPlugin("codefly.dev", name)
			require.True(t, ok)
			agent := plugin.Agent()
			connection, err := manager.Load(ctx, agent, manager.WithoutSandbox(), manager.WithoutPrincipal(), manager.WithEnv("DOCKER_HOST=unix:///nonexistent-recipe-test-docker.sock"))
			require.NoError(t, err)
			t.Cleanup(connection.Close)
			observed := &observedRecipeClient{BuilderClient: builderv0.NewBuilderClient(connection.GRPCConn())}
			client := &coreservices.BuilderAgent{BuilderClient: observed}
			root := t.TempDir()
			service := &resources.Service{Name: "api", Version: "1.0.0", Agent: agent}
			service.WithDir(root)
			require.NoError(t, service.Save(ctx))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "code"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(root, "code", "go.mod"), []byte("module example.com/recipe\n\ngo 1.27.0\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(root, "code", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))
			_, err = client.Load(ctx, &builderv0.LoadRequest{Identity: &basev0.ServiceIdentity{Name: "api", Module: "app", Version: "1.0.0", WorkspacePath: root, RelativeToWorkspace: "."}, CreationMode: &builderv0.CreationMode{Communicate: false}})
			require.NoError(t, err)
			flow := &Flow{world: &World{Workspace: &resources.Workspace{Name: "workspace"}}}
			cache := &builderv0.BuildCacheOptions{Backend: "registry", Scope: "protected", Imports: []string{"ghcr.io/org/cache"}, Exports: []string{"ghcr.io/org/cache"}}
			flow.WithBuildCache(cache)
			flow.WithBuildxBuilder("selected")
			instance := &services.Instance{Service: service, Identity: &resources.ServiceIdentity{Name: "api", Module: "app"}}
			instance.Builder = &services.BuilderInstance{Instance: instance, Builder: client}
			build, err := NewBuilder(ctx, instance, flow.world)
			require.NoError(t, err)
			t.Setenv("PATH", t.TempDir())
			_, err = build.Build(ctx)
			require.ErrorIs(t, err, exec.ErrNotFound)
			require.NotNil(t, observed.request)
			output, err := buildRecipeOutputDirectory(root)
			require.NoError(t, err)
			require.Equal(t, output, observed.request.GetOutputDirectory())
			docker := observed.request.GetBuildContext().GetDockerBuildContext()
			require.Equal(t, "selected", docker.GetBuildxBuilder())
			require.Equal(t, `["protected","workspace","app/api",""]`, docker.GetCache().GetScope())
			require.Equal(t, cache.Imports, docker.GetCache().GetImports())
			require.Equal(t, cache.Exports, docker.GetCache().GetExports())
			require.True(t, proto.Equal(cache, flow.world.BuildCache))
			require.Equal(t, builderv0.BuildStatus_SUCCESS, observed.response.GetState().GetState())
			plan := observed.response.GetResult().GetDockerBuildPlan()
			require.NotNil(t, plan)
			require.NoError(t, coreservices.VerifyDockerBuildPlan(output, plan))
		})
	}
}
