package orchestration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/contract"
	agentservices "github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func localRuntimeBuilder(t *testing.T, harness *imageCacheHarness) *Builder {
	t.Helper()
	workspace := &resources.Workspace{Name: "platform"}
	workspace.WithDir(harness.workspaceDir)
	service := &resources.Service{
		Name: "api", Version: "1.0.0",
		Agent: &resources.Agent{Publisher: "codefly.dev", Name: "test-peer", Version: "0.1.0"},
	}
	service.WithDir(harness.serviceDir)
	instance := &services.Instance{Service: service, Identity: &resources.ServiceIdentity{Name: "api", Module: "saas"}}
	declaration := contract.Current()
	declaration.Capabilities = append(declaration.Capabilities, contract.RuntimeInitImage)
	instance.Info = &agentv0.AgentInformation{Contract: declaration}
	instance.Builder = &services.BuilderInstance{Instance: instance, Builder: harness.client}
	b, err := NewBuilder(t.Context(), instance, &World{
		Workspace: workspace, BuildxBuilder: "stub", RebuildImages: harness.rebuild,
		hostArchitecture: func() (string, error) { return "arm64", nil },
	})
	require.NoError(t, err)
	return b
}

type runtimeImageBuilderPeer struct {
	imageCachePeer
	stock bool
}

func (*runtimeImageBuilderPeer) Load(context.Context, *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	return &builderv0.LoadResponse{State: &builderv0.LoadStatus{State: builderv0.LoadStatus_READY}}, nil
}

func (*runtimeImageBuilderPeer) Init(context.Context, *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	return &builderv0.InitResponse{State: &builderv0.InitStatus{State: builderv0.InitStatus_SUCCESS}}, nil
}

func (peer *runtimeImageBuilderPeer) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	if peer.stock {
		return &builderv0.BuildResponse{
			State:         &builderv0.BuildStatus{State: builderv0.BuildStatus_SUCCESS},
			BuildxBuilder: req.GetBuildContext().GetDockerBuildContext().GetBuildxBuilder(),
		}, nil
	}
	return peer.imageCachePeer.Build(ctx, req)
}

func runtimeImageRunner(t *testing.T, peer *runtimeImageBuilderPeer) (*Runner, *dockerStub) {
	t.Helper()
	stub := installDockerStub(t, true)
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentInputs))
	root := t.TempDir()
	world.Workspace.WithDir(root)
	serviceDir := filepath.Join(root, "modules/web/services/gateway")
	require.NoError(t, os.MkdirAll(filepath.Join(serviceDir, "code"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(serviceDir, "code/payload"), []byte("runtime input\n"), 0o644))
	runner.instance.Service.WithDir(serviceDir)
	runner.instance.Module.WithDir(filepath.Join(root, "modules/web"))
	world.hostArchitecture = func() (string, error) { return "arm64", nil }
	world.BuildxBuilder = "stub"
	runner.runtimeContext = resources.RuntimeContextContainer
	runner.containerRecoveryIdentity = "test-runtime-image-scope"
	runner.instance.ContainerRecoveryScope = runner.containerRecoveryIdentity
	declaration := contract.Current()
	declaration.Capabilities = append(declaration.Capabilities, contract.RuntimeInitImage)
	runner.instance.Info = &agentv0.AgentInformation{Contract: declaration}
	runner.instance.Capabilities = []*agentv0.Capability{{Type: agentv0.Capability_BUILDER}}
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, peer)
	conn, err := grpc.NewClient(serveGRPC(t, server), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	runner.instance.Builder = &services.BuilderInstance{
		Instance: runner.instance,
		Builder:  &agentservices.BuilderAgent{BuilderClient: builderv0.NewBuilderClient(conn)},
	}
	return runner, stub
}

// Exercise the real Runner.Init, builder gRPC lifecycle and buildx boundary,
// then read the image the runtime agent received across a process boundary.
func TestRunnerBuildsAndHandsOffLocalRuntimeImage(t *testing.T) {
	runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
	_, err := runner.Init(t.Context())
	require.NoError(t, err)
	first := exposedValues(t.Context(), t, runner.world)["runtime-image"]
	require.Regexp(t, `^codefly-local/runtime:sha256-[0-9a-f]{64}$`, first)
	require.Equal(t, 1, stub.calls(t, "build"))
	_, err = runner.Init(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, exposedValues(t.Context(), t, runner.world)["runtime-image"])
	require.Equal(t, 1, stub.calls(t, "build"))
}

func TestRunnerRefusesImageHandoffWithoutLiveCapability(t *testing.T) {
	runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
	runner.instance.Info.Contract = contract.Current()
	_, err := runner.Init(t.Context())
	require.ErrorContains(t, err, contract.RuntimeInitImage)
	require.Zero(t, stub.calls(t, "build"))
}

func TestRunnerStockImageNeedsNoHandoffCapability(t *testing.T) {
	runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{stock: true})
	runner.instance.Info.Contract = contract.Current()
	_, err := runner.Init(t.Context())
	require.NoError(t, err)
	require.NotContains(t, exposedValues(t.Context(), t, runner.world), "runtime-image")
	require.Zero(t, stub.calls(t, "build"))
}

func TestRunnerNativeRuntimeDoesNotBuildImage(t *testing.T) {
	runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
	runner.runtimeContext = resources.RuntimeContextNative
	_, err := runner.Init(t.Context())
	require.NoError(t, err)
	require.NotContains(t, exposedValues(t.Context(), t, runner.world), "runtime-image")
	require.Zero(t, stub.calls(t, "build"))
}

func TestRunnerLeavesExplicitImageValidationToAgent(t *testing.T) {
	for _, image := range []string{"example.test/runtime:v1", "example.test/runtime:latest", "malformed image"} {
		t.Run(image, func(t *testing.T) {
			runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
			runner.instance.Service.Spec = map[string]any{"runtime-image": image}
			runner.instance.Info.Contract = contract.Current()
			_, err := runner.Init(t.Context())
			require.NoError(t, err) // this test peer echoes; real agents own validation
			require.NotContains(t, exposedValues(t.Context(), t, runner.world), "runtime-image")
			require.Zero(t, stub.calls(t, "build"))
		})
	}
}

// Opt-in host-boundary integration: real buildx builds/loads an offline scratch
// image, and Runtime.Init receives the reference through the gRPC process peer.
// This deliberately does not stand in for the platform-obin composition boot.
func TestLocalRuntimeImageDockerHandoff(t *testing.T) {
	if os.Getenv("CODEFLY_TEST_RUNTIME_IMAGE") != "1" {
		t.Skip("set CODEFLY_TEST_RUNTIME_IMAGE=1 to exercise real buildx and the image handoff")
	}
	docker, err := exec.LookPath("docker")
	require.NoError(t, err)
	runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
	runner.world.BuildxBuilder = ""
	runner.world.hostArchitecture = nil
	// Count actual build invocations while forwarding every command to Docker.
	t.Setenv("CODEFLY_TEST_REAL_DOCKER", docker)
	t.Setenv("CODEFLY_TEST_DOCKER_CALLS", filepath.Join(stub.dir, "calls"))
	require.NoError(t, os.WriteFile(filepath.Join(stub.dir, "docker"), []byte(`#!/bin/sh
if [ "$1 $2" = "buildx build" ]; then printf 'build\n' >> "$CODEFLY_TEST_DOCKER_CALLS"; fi
exec "$CODEFLY_TEST_REAL_DOCKER" "$@"
`), 0o755))
	// Give this fixture its own input-derived tag, so cleanup touches no image
	// another invocation could be using.
	require.NoError(t, os.WriteFile(filepath.Join(runner.instance.Service.Dir(), "code/payload"), []byte(t.TempDir()), 0o644))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	_, err = runner.Init(ctx)
	require.NoError(t, err)
	image := exposedValues(ctx, t, runner.world)["runtime-image"]
	require.NotEmpty(t, image)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		output, removeErr := exec.CommandContext(cleanup, docker, "image", "rm", image).CombinedOutput()
		require.NoError(t, removeErr, "%s", output)
	})
	imageID, err := inspectLocalImageID(ctx, image)
	require.NoError(t, err)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, imageID)
	require.Equal(t, 1, stub.calls(t, "build"))
	_, err = runner.Init(ctx)
	require.NoError(t, err)
	require.Equal(t, image, exposedValues(ctx, t, runner.world)["runtime-image"])
	require.Equal(t, 1, stub.calls(t, "build"), "a second Init must reuse the loaded real image")
}

func TestLocalRuntimeImageReusesRecipeAndInvalidatesSourceChanges(t *testing.T) {
	h := newImageCacheHarness(t, false, true)
	build := func() string {
		image, err := localRuntimeBuilder(t, h).buildLocalRuntimeImage(t.Context())
		require.NoError(t, err)
		require.Regexp(t, `^codefly-local/runtime:sha256-[0-9a-f]{64}$`, image)
		return image
	}
	first := build()
	require.Equal(t, 1, h.docker.calls(t, "build"))
	require.Equal(t, first, build())
	require.Equal(t, 1, h.docker.calls(t, "build"), "unchanged inputs must not invoke buildx again")
	h.write(t, filepath.Join(h.serviceDir, "notes.md"), "ignored edit\n")
	require.Equal(t, first, build())
	require.Equal(t, 1, h.docker.calls(t, "build"))
	h.write(t, filepath.Join(h.serviceDir, "code", "main.go"), "package main\nfunc main() { println(42) }\n")
	second := build()
	require.NotEqual(t, first, second)
	require.Equal(t, 2, h.docker.calls(t, "build"))
	require.Equal(t, second, build())
	require.Equal(t, 2, h.docker.calls(t, "build"))
	// A forced build retains the recipe-derived name, never a clock-based tag.
	h.rebuild = true
	require.Equal(t, second, build())
	require.Equal(t, 3, h.docker.calls(t, "build"))
	require.NoDirExists(t, filepath.Join(h.serviceDir, "builder"))
	require.NoDirExists(t, filepath.Join(h.serviceDir, "build-recipes"))
	manifest := filepath.Join(h.workspaceDir, ".codefly/runtime-build/saas/api/build-recipes/0.1.0/recipe.codefly.json")
	require.Contains(t, h.read(t, manifest), second)
	require.Contains(t, h.read(t, manifest), "linux/arm64")
}

func TestLocalRuntimeImageIdentityDoesNotIncludeCheckoutPath(t *testing.T) {
	h := newImageCacheHarness(t, false, true)
	first, err := localRuntimeBuilder(t, h).buildLocalRuntimeImage(t.Context())
	require.NoError(t, err)
	other := newImageCacheHarness(t, false, true)
	second, err := localRuntimeBuilder(t, other).buildLocalRuntimeImage(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestLocalRuntimeImageTagBindsBuildArgumentsAndRecipeBytes(t *testing.T) {
	h := newImageCacheHarness(t, false, true)
	build := func(argument, dockerfile string) string {
		b := localRuntimeBuilder(t, h)
		b.localRuntime = true
		_, err := b.Plan(t.Context())
		require.NoError(t, err)
		plan := b.planned.response.GetResult().GetDockerBuildPlan()
		plan.Recipes[0].BuildArgs = map[string]string{"MODE": argument}
		if dockerfile != "" {
			h.write(t, filepath.Join(b.planned.outputDir, "Dockerfile"), dockerfile)
		}
		plan, err = agentservices.BuildEmittedDockerBuildPlan(b.planned.outputDir, plan.Recipes, []string{"Dockerfile", "dockerignore"})
		require.NoError(t, err)
		b.planned.response.Result = &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildPlan{DockerBuildPlan: plan}}
		image, err := b.buildLocalRuntimeImage(t.Context())
		require.NoError(t, err)
		return image
	}
	first := build("one", "")
	require.Equal(t, first, build("one", ""))
	second := build("two", "")
	require.NotEqual(t, first, second)
	third := build("two", "FROM scratch\nCOPY code /changed\n")
	require.NotEqual(t, second, third)
	require.Equal(t, third, build("two", "FROM scratch\nCOPY code /changed\n"))
	require.Equal(t, 3, h.docker.calls(t, "build"))
}

func TestLocalRuntimeImageRefusesChangedContextDuringBuild(t *testing.T) {
	h := newImageCacheHarness(t, false, true)
	h.docker.editDuringBuild(t, filepath.Join(h.serviceDir, "code/main.go"), "changed during build\n")
	image, err := localRuntimeBuilder(t, h).buildLocalRuntimeImage(t.Context())
	require.ErrorContains(t, err, "inputs changed while building")
	require.Empty(t, image)
	entries, err := os.ReadDir(filepath.Join(h.workspaceDir, imageCacheDir))
	if !os.IsNotExist(err) {
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func TestLocalRuntimeImageRejectsPushAndUnsupportedPlatform(t *testing.T) {
	h := newImageCacheHarness(t, false, true)
	b := localRuntimeBuilder(t, h)
	b.world.Push = true
	_, err := b.buildLocalRuntimeImage(t.Context())
	require.ErrorContains(t, err, "must be loaded")
	b.world.Push = false
	b.world.hostArchitecture = func() (string, error) { return "riscv64", nil }
	_, err = b.buildLocalRuntimeImage(t.Context())
	require.ErrorContains(t, err, "requires linux/riscv64")
	require.Zero(t, h.docker.calls(t, "build"))
}
