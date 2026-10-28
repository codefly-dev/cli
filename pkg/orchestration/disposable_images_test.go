package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/architecture"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

func disposableGraphWorkspace(t *testing.T, edges map[string]string) *resources.Workspace {
	t.Helper()
	files := map[string]string{"workspace.codefly.yaml": "name: disposable\nlayout: modules\nmodules:\n  - name: app\n", "modules/app/module.codefly.yaml": "name: app\nservices:\n  - name: origin\n  - name: dependency\n  - name: producer\n  - name: unrelated\n"}
	for _, name := range []string{"origin", "dependency", "producer", "unrelated"} {
		files["modules/app/services/"+name+"/service.codefly.yaml"] = "name: " + name + "\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: codefly.dev\n  name: missing-disposable-peer\n  version: 0.0.1\n" + edges[name]
	}
	return writeTempWorkspace(t, files)
}

func disposableEdge(name, kind string) string {
	s := "service-dependencies:\n  - name: " + name + "\n"
	if kind != "" {
		s += "    kind: " + kind + "\n"
	}
	return s
}

func TestDisposableImagesAdmitSelectedRuntimeClosure(t *testing.T) {
	for _, kind := range []string{"build", "schema", "", "runtime", "completion"} {
		t.Run("kind="+kind, func(t *testing.T) {
			ws := disposableGraphWorkspace(t, map[string]string{"origin": disposableEdge("dependency", "runtime"), "dependency": disposableEdge("producer", kind)})
			deps, err := architecture.NewServiceDependencies(t.Context(), ws)
			require.NoError(t, err)
			err = checkDisposableImageDependencies(t.Context(), deps, []string{"app/origin"})
			if kind == "runtime" || kind == "completion" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "app/dependency has build prerequisite app/producer")
			}
		})
	}
	ws := disposableGraphWorkspace(t, map[string]string{"unrelated": disposableEdge("producer", "build")})
	deps, err := architecture.NewServiceDependencies(t.Context(), ws)
	require.NoError(t, err)
	require.NoError(t, checkDisposableImageDependencies(t.Context(), deps, []string{"app/origin"}))
	require.ErrorContains(t, checkDisposableImageDependencies(t.Context(), deps, []string{"app/origin", "app/unrelated"}), "build prerequisite")
	module, err := ws.LoadModuleFromName(t.Context(), "app")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "origin")
	require.NoError(t, err)
	env, err := SelectEnvironment(ws, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(t.Context(), ws, module, service, env, TestMode)
	require.NoError(t, err)
	flow.WithDisposableImages()
	flow.coRoots = []string{"app/unrelated"}
	require.ErrorContains(t, flow.InitManagers(t.Context()), "build prerequisite")
	require.Nil(t, flow.hub, "refusal precedes even origin manager creation")
}

// Uses a real loopback recipe peer. No Docker CLI reaches an engine: every
// invocation runs installDockerStub's isolated executable.
type disposableRecipePeer struct {
	sbomRequests chan *builderv0.SBOMRequest
	imageCachePeer
	multiple bool
	unknown  bool
	calls    atomic.Int32
}

func (p *disposableRecipePeer) Build(ctx context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	p.calls.Add(1)
	response, err := p.imageCachePeer.Build(ctx, req)
	if err != nil {
		return nil, err
	}
	plan := response.GetResult().GetDockerBuildPlan()
	if p.multiple {
		second := proto.CloneOf(plan.Recipes[0])
		second.Name = "second"
		second.Image = "example.test/second:v1"
		plan.Recipes = append(plan.Recipes, second)
	}
	if p.unknown {
		if err := os.WriteFile(filepath.Join(req.OutputDirectory, "Dockerfile"), []byte("ARG BASE\nFROM ${BASE}\n"), 0600); err != nil {
			return nil, err
		}
	}
	plan, err = coreservices.BuildEmittedDockerBuildPlan(req.OutputDirectory, plan.Recipes, []string{"Dockerfile", "dockerignore"})
	if err != nil {
		return nil, err
	}
	response.Result = &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildPlan{DockerBuildPlan: plan}}
	return response, nil
}

func disposableBuilder(t *testing.T, p *disposableRecipePeer) (*Builder, *imageCacheHarness) {
	t.Helper()
	h := newImageCacheHarness(t, false, true)
	b := localRuntimeBuilder(t, h)
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, p)
	conn, err := grpc.NewClient(serveGRPC(t, server), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	b.instance.Builder = &services.BuilderInstance{Instance: b.instance, Builder: &coreservices.BuilderAgent{BuilderClient: builderv0.NewBuilderClient(conn)}}
	(&Flow{world: b.world}).WithDisposableImages()
	return b, h
}

func TestDisposableImagesRejectPublicationBeforeRPCAndRecheck(t *testing.T) {
	for _, kind := range []string{"push", "cache export"} {
		t.Run(kind, func(t *testing.T) {
			p := &disposableRecipePeer{}
			b, h := disposableBuilder(t, p)
			reject := func() {
				if kind == "push" {
					b.world.Push = true
				} else {
					b.world.BuildCache = &builderv0.BuildCacheOptions{Exports: []string{"example.test/cache"}}
				}
			}
			reject()
			_, err := b.Plan(t.Context())
			require.ErrorContains(t, err, "push or build-cache exports")
			require.Zero(t, p.calls.Load())
			require.Zero(t, h.docker.calls(t, "build"))
			b.world.Push = false
			b.world.BuildCache = nil
			_, err = b.Plan(t.Context())
			require.NoError(t, err)
			plan := b.planned
			reject()
			_, err = b.Build(t.Context())
			require.ErrorContains(t, err, "push or build-cache exports")
			err = b.buildFromPlan(t.Context(), plan.outputDir, plan.response.GetResult().GetDockerBuildPlan(), nil, nil)
			require.ErrorContains(t, err, "push or build-cache exports")
			require.EqualValues(t, 1, p.calls.Load())
			require.Zero(t, h.docker.calls(t, "build"))
		})
	}
}

func TestDisposableImagesRejectMultiRecipeBeforePrefetchAndProvision(t *testing.T) {
	p := &disposableRecipePeer{imageCachePeer: imageCachePeer{goModules: true}, multiple: true}
	b, h := disposableBuilder(t, p)
	b.world.BuildxBuilder = ""
	b.world.BuildCache = &builderv0.BuildCacheOptions{Backend: "registry", Scope: "test", Imports: []string{"example.test/cache"}}
	prefetch := newGoModulePrefetch()
	var calls []goCall
	prefetch.run = recordingGo(&calls, nil)
	b.world.goModulePrefetch = prefetch
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	_, err := b.Plan(t.Context())
	require.ErrorContains(t, err, "has 2 recipes")
	require.Empty(t, calls)
	require.NoFileExists(t, filepath.Join(h.docker.dir, "calls"))
	output := t.TempDir()
	response, err := p.Build(t.Context(), &builderv0.BuildRequest{OutputDirectory: output})
	require.NoError(t, err)
	err = b.buildFromPlan(t.Context(), output, response.GetResult().GetDockerBuildPlan(), nil, nil)
	require.ErrorContains(t, err, "has 2 recipes")
	require.NoFileExists(t, filepath.Join(h.docker.dir, "calls"))
}

func TestDisposableImagesRefuseUnknownIdentityInPlanAndBuild(t *testing.T) {
	for _, prefetch := range []bool{false, true} {
		t.Run(fmt.Sprint(prefetch), func(t *testing.T) {
			b, h := disposableBuilder(t, &disposableRecipePeer{imageCachePeer: imageCachePeer{goModules: prefetch}, unknown: true})
			b.world.RebuildImages = true
			_, err := b.Plan(t.Context())
			if !prefetch {
				require.NoError(t, err)
				_, err = b.Build(t.Context())
			}
			require.ErrorContains(t, err, "cannot identify disposable image inputs")
			require.Zero(t, h.docker.calls(t, "build"))
			require.Zero(t, h.docker.calls(t, "imagetools"))
		})
	}
}

func executeDisposablePlan(t *testing.T, b *Builder) *builderv0.DockerBuildPlan {
	t.Helper()
	_, err := b.Plan(t.Context())
	require.NoError(t, err)
	planned := b.planned
	original := planned.response.GetResult().GetDockerBuildPlan()
	before := proto.CloneOf(original)
	require.NoError(t, b.buildFromPlan(t.Context(), planned.outputDir, original, planned.prefetched, planned.reuse))
	require.True(t, proto.Equal(before, original), "verified producer plan remains unchanged")
	execution := b.imageEvidencePlan(original)
	b.planned = nil
	return execution
}

func TestDisposableImagesProjectionCacheAndActualSubjects(t *testing.T) {
	b, h := disposableBuilder(t, &disposableRecipePeer{})
	b.world.CollectImageSBOM = true
	first := executeDisposablePlan(t, b)
	image := first.Recipes[0].Image
	require.Contains(t, image, "codefly-local/disposable/")
	require.Equal(t, 1, h.docker.calls(t, "build"))
	assertSubject := func(plan *builderv0.DockerBuildPlan) {
		subjects, err := producedSubjects(b.instance.Unique(), plan, false, b.resolvedImages)
		require.NoError(t, err)
		require.Len(t, subjects, 1)
		require.Equal(t, image, subjects[0].Reference)
		require.Equal(t, first.Recipes[0].Platforms[0], subjects[0].Platform)
		require.Equal(t, fmt.Sprintf("sha256:%064d", 7), subjects[0].Digest)
	}
	assertSubject(first)
	key := h.key(t)
	entry, found := b.imageBuildCache().lookup(key, b.instance.Unique(), false)
	require.True(t, found)
	require.Equal(t, image, entry.Image)
	b.resolvedImages = nil
	again := executeDisposablePlan(t, b)
	require.Equal(t, image, again.Recipes[0].Image)
	require.Equal(t, 1, h.docker.calls(t, "build"))
	assertSubject(again)
	// Same key and loaded ID cannot authorize a differently named cache entry.
	entry.Image = "example.test/foreign:retained"
	require.NoError(t, b.imageBuildCache().store(t.Context(), entry))
	executeDisposablePlan(t, b)
	require.Equal(t, 2, h.docker.calls(t, "build"))
	for _, args := range h.docker.buildArgs(t) {
		require.Contains(t, args, "-t "+image)
		require.NotContains(t, args, "-t example.test/")
	}
	previousScope := b.world.disposableImageScope
	(&Flow{world: b.world}).WithDisposableImages()
	require.Equal(t, previousScope, b.world.disposableImageScope)
	b.world.disposableImageScope = NewInvocationID()
	different := executeDisposablePlan(t, b)
	require.NotEqual(t, image, different.Recipes[0].Image)
	require.Equal(t, 3, h.docker.calls(t, "build"))
}

func TestDisposableImagesRetainPlanProjectionBeforePrefetchIdentity(t *testing.T) {
	b, h := disposableBuilder(t, &disposableRecipePeer{imageCachePeer: imageCachePeer{goModules: true}})
	prefetch := newGoModulePrefetch()
	var calls []goCall
	prefetch.run = recordingGo(&calls, nil)
	b.world.goModulePrefetch = prefetch
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	_, err := b.Plan(t.Context())
	require.NoError(t, err)
	planned := b.planned
	recipe := planned.response.GetResult().GetDockerBuildPlan().Recipes[0]
	projection := b.executionRecipes[recipe.Name]
	decision := planned.reuse[recipe.Name]
	require.Equal(t, projection.Image, decision.image)
	require.NotEmpty(t, decision.identity.Key)
	require.NotEmpty(t, calls)
	require.NoError(t, b.buildFromPlan(t.Context(), planned.outputDir, planned.response.GetResult().GetDockerBuildPlan(), planned.prefetched, planned.reuse))
	require.Same(t, projection, b.executionRecipes[recipe.Name])
	require.Contains(t, h.docker.buildArgs(t)[0], "-t "+projection.Image)
}

func TestDisposableRuntimeImagesKeepCanonicalHandoffAndMutationRefusal(t *testing.T) {
	runner, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
	(&Flow{world: runner.world}).WithDisposableImages()
	_, err := runner.Init(t.Context())
	require.NoError(t, err)
	ref := exposedValues(t.Context(), t, runner.world)["runtime-image"]
	require.Contains(t, ref, "docker.io/codefly-local/disposable/"+runner.world.disposableImageScope+"@sha256:")
	_, err = runner.Init(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, stub.calls(t, "build"))
	// A second service in the same World cannot choose the origin destination.
	b, err := NewBuilder(t.Context(), &services.Instance{Identity: &resources.ServiceIdentity{Module: "other", Name: "dependency"}}, runner.world)
	require.NoError(t, err)
	projection := b.scopedRecipe(&builderv0.DockerBuildRecipe{Name: "app", Image: "example.test/app:v1"})
	require.NotContains(t, stub.buildArgs(t)[0], "-t "+projection.Image)
	h := newImageCacheHarness(t, false, true)
	local := localRuntimeBuilder(t, h)
	(&Flow{world: local.world}).WithDisposableImages()
	h.docker.editDuringBuild(t, filepath.Join(h.serviceDir, "code/main.go"), "changed during build\n")
	image, err := local.buildLocalRuntimeImage(t.Context())
	require.ErrorContains(t, err, "inputs changed while building")
	require.Empty(t, image)
}

func TestDisposableImagesProvisionReadOnlyCacheOnlyAfterAdmission(t *testing.T) {
	b, h := disposableBuilder(t, &disposableRecipePeer{})
	b.world.BuildxBuilder = ""
	b.world.BuildCache = &builderv0.BuildCacheOptions{Backend: "registry", Scope: "fixture", Imports: []string{"example.test/cache"}}
	// Extend the isolated fake only; no real Docker executable is invoked.
	path := filepath.Join(h.docker.dir, "docker")
	script, err := os.ReadFile(path)
	require.NoError(t, err)
	script = []byte(strings.Replace(string(script), "case \"$1 $2\" in", `case "$1 $2" in
"buildx inspect") record provision-inspect; exit 1 ;;
"buildx create") record provision-create; exit 0 ;;`, 1))
	require.NoError(t, os.WriteFile(path, script, 0700))
	_, err = b.Plan(t.Context())
	require.NoError(t, err)
	require.Zero(t, h.docker.calls(t, "provision-create"))
	executeDisposablePlan(t, b)
	require.Equal(t, 1, h.docker.calls(t, "provision-create"))
	require.Equal(t, 1, h.docker.calls(t, "build"))
	executeDisposablePlan(t, b)
	require.Equal(t, 1, h.docker.calls(t, "provision-create"))
	require.Equal(t, 1, h.docker.calls(t, "build"))
}

func TestDisposableImagesRejectInvalidRetainedDecision(t *testing.T) {
	b, h := disposableBuilder(t, &disposableRecipePeer{})
	_, err := b.Plan(t.Context())
	require.NoError(t, err)
	p := b.planned
	plan := p.response.GetResult().GetDockerBuildPlan()
	err = b.buildFromPlan(t.Context(), p.outputDir, plan, nil, map[string]imageReuse{plan.Recipes[0].Name: {image: "example.test/shared:v1"}})
	require.ErrorContains(t, err, "does not match")
	require.Zero(t, h.docker.calls(t, "build"))
}

func TestDisposableImagesCheckConfigurationReferenceParticipants(t *testing.T) {
	ws := overrideOrderingWorkspace(t)
	path := filepath.Join(ws.Dir(), "modules/platform/services/authority/service.codefly.yaml")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = file.WriteString("service-dependencies:\n  - name: gateway\n    kind: build\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	module, err := ws.LoadModuleFromName(t.Context(), "payments")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "worker")
	require.NoError(t, err)
	env, err := SelectEnvironment(ws, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(t.Context(), ws, module, service, env, TestMode)
	require.NoError(t, err)
	flow.WithDisposableImages()
	require.ErrorContains(t, flow.InitManagers(t.Context()), "platform/authority has build prerequisite platform/gateway")
	require.Nil(t, flow.hub)
}

func TestDisposableImagesRespectSelectedCutoffs(t *testing.T) {
	ws := disposableGraphWorkspace(t, map[string]string{"origin": disposableEdge("dependency", "runtime"), "dependency": disposableEdge("producer", "build")})
	deps, err := architecture.NewServiceDependencies(t.Context(), ws, architecture.SkipDependencyFor("app/dependency"))
	require.NoError(t, err)
	require.NoError(t, checkDisposableImageDependencies(t.Context(), deps, []string{"app/origin"}))
	deps, err = architecture.NewServiceDependencies(t.Context(), ws, architecture.ExcludeServices("app/dependency"))
	require.NoError(t, err)
	require.NoError(t, checkDisposableImageDependencies(t.Context(), deps, []string{"app/origin"}))
}

func TestDisposableRuntimePolicyReachesOriginAndDependencyInEveryMode(t *testing.T) {
	for _, mode := range []Mode{LintMode, CompileMode, TestMode} {
		t.Run(string(mode), func(t *testing.T) {
			origin, _ := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
			origin.world.Mode = mode
			(&Flow{world: origin.world}).WithDisposableImages()
			dependency, stub := runtimeImageRunner(t, &runtimeImageBuilderPeer{})
			dependency.instance.Identity.Name = "dependency"
			dependency.instance.Service.Name = "dependency"
			dependency.world = origin.world
			// The second isolated Docker stub remains first on PATH; keep both runners
			// on that same fake engine, exactly as their shared World does in a flow.
			a, err := origin.localRuntimeImage(t.Context())
			require.NoError(t, err)
			d, err := dependency.localRuntimeImage(t.Context())
			require.NoError(t, err)
			require.Contains(t, a, "/disposable/"+origin.world.disposableImageScope+"@sha256:")
			require.Contains(t, d, "/disposable/"+origin.world.disposableImageScope+"@sha256:")
			args := stub.buildArgs(t)
			require.Len(t, args, 2)
			require.NotEqual(t, strings.Split(strings.Split(args[0], " -t ")[1], " ")[0], strings.Split(strings.Split(args[1], " -t ")[1], " ")[0])
			_, err = origin.localRuntimeImage(t.Context())
			require.NoError(t, err)
			require.Equal(t, 2, stub.calls(t, "build"))
		})
	}
}

func (p *disposableRecipePeer) SBOM(_ context.Context, request *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	if p.sbomRequests != nil {
		p.sbomRequests <- proto.CloneOf(request)
	}
	response := &builderv0.SBOMResponse{State: &builderv0.SBOMStatus{State: builderv0.SBOMStatus_COMPLETE}, Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE}
	for _, subject := range request.Subjects {
		response.Images = append(response.Images, &builderv0.ImageSBOM{Digest: subject.Digest, Platform: subject.Platform, Subjects: []*builderv0.ImageSubject{proto.CloneOf(subject)}, Bom: &agentv0.Bom{Components: []*agentv0.Component{{Name: "test-only-inventory"}}}, Tool: "test-peer", Sha256: "test-only-checksum"})
	}
	return response, nil
}

func TestDisposableImagesActualSBOMRequestFreshAndReused(t *testing.T) {
	peer := &disposableRecipePeer{sbomRequests: make(chan *builderv0.SBOMRequest, 2)}
	b, h := disposableBuilder(t, peer)
	b.world.CollectImageSBOM = true
	var previous *builderv0.ImageSubject
	for i := 0; i < 2; i++ {
		_, err := b.Plan(t.Context())
		require.NoError(t, err)
		original := b.planned.response.GetResult().GetDockerBuildPlan()
		before := proto.CloneOf(original)
		_, err = b.Build(t.Context())
		require.NoError(t, err)
		require.True(t, proto.Equal(before, original))
		request := <-peer.sbomRequests
		require.Len(t, request.Subjects, 1)
		subject := request.Subjects[0]
		require.Contains(t, subject.Reference, "codefly-local/disposable/"+b.world.disposableImageScope+":")
		require.NotEqual(t, original.Recipes[0].Image, subject.Reference)
		require.Equal(t, original.Recipes[0].Platforms[0], subject.Platform)
		require.Equal(t, fmt.Sprintf("sha256:%064d", 7), subject.Digest)
		require.Len(t, b.ImageEvidence(), 1)
		if previous != nil {
			require.True(t, proto.Equal(previous, subject))
		}
		previous = subject
	}
	require.Equal(t, 1, h.docker.calls(t, "build"), "second SBOM covers an actual cache hit")
}

func TestDisposableImagesRebuildMissingImageWithinScope(t *testing.T) {
	b, h := disposableBuilder(t, &disposableRecipePeer{})
	first := executeDisposablePlan(t, b)
	path := filepath.Join(h.docker.dir, "docker")
	script, err := os.ReadFile(path)
	require.NoError(t, err)
	// One absent-image read makes the cache miss. Later inspection after the
	// fake build succeeds, so the missing image cannot cause an original-tag fallback.
	script = []byte(strings.Replace(string(script), `"image inspect")`, `"image inspect")
  if [ -f "$STATE/missing" ]; then rm "$STATE/missing"; exit 1; fi`, 1))
	require.NoError(t, os.WriteFile(path, script, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(h.docker.dir, "missing"), nil, 0600))
	again := executeDisposablePlan(t, b)
	require.Equal(t, first.Recipes[0].Image, again.Recipes[0].Image)
	require.Equal(t, 2, h.docker.calls(t, "build"))
	b.world.RebuildImages = true
	forced := executeDisposablePlan(t, b)
	require.Equal(t, first.Recipes[0].Image, forced.Recipes[0].Image)
	require.Equal(t, 3, h.docker.calls(t, "build"))
}
