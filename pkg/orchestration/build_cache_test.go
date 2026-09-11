package orchestration

import (
	"context"
	"fmt"
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
