package orchestration

// Opt-in host-boundary qualification. No deletion is registered, even on
// failure. The proxy permits only scoped builds and read-only engine queries.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/core/agents/manager"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var disposableProtectedImages = map[string]string{
	"codefly-dev/app/subject:0.0.0": "sha256:7bef274c8e7c158a93bd5fcc56f39d530f3e56826cfa34151899747d1ffa561c",
	"codefly-local/runtime:sha256-5ac8c7280c1eca0c57ccb065cb29216316c46fe5cbd844e6695e4736c10a1c6b": "sha256:714e6933800934c51975fa2aa49206d01f9a43435ab158a0936ac50b0a5a4e90",
}

type disposableEngineCall struct {
	Args       []string
	Start, End int64
	Error      string
}

// Runs only as the Docker executable of an explicitly opted-in test. Exiting
// prevents the Go test runner from appending PASS to Docker's protocol output.
func TestDisposableDockerProxy(t *testing.T) {
	if os.Getenv("CODEFLY_TEST_DOCKER_PROXY_CHILD") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		os.Exit(97)
	}
	args = args[1:]
	build := len(args) > 1 && args[0] == "buildx" && args[1] == "build"
	if !allowedDisposableEngineCall(args) {
		fmt.Fprintln(os.Stderr, "unallocated engine operation", args)
		os.Exit(97)
	}
	if build {
		// Every actual build in this selection is fresh. Same-flow reuse must
		// issue no build. Existing output is a collision, never setup authority.
		ref := disposableBuiltRef(disposableEngineCall{Args: args})
		probe := exec.Command(os.Getenv("CODEFLY_TEST_DISPOSABLE_REAL_DOCKER"), "image", "inspect", "--format", "{{.Id}}", ref)
		output, err := probe.CombinedOutput()
		if err == nil || !strings.Contains(strings.ToLower(string(output)), "no such image") {
			fmt.Fprintln(os.Stderr, "fresh fixture destination is not proven absent", ref)
			os.Exit(97)
		}
	}
	call := disposableEngineCall{Args: args, Start: time.Now().UnixNano()}
	// Two separate CLI processes meet at their first owned build. The actual
	// command intervals below, not the barrier alone, must overlap.
	if build && os.Getenv("CODEFLY_TEST_IMAGE_BARRIER") != "" {
		dir := os.Getenv("CODEFLY_TEST_IMAGE_BARRIER")
		_ = os.WriteFile(filepath.Join(dir, os.Getenv("CODEFLY_TEST_IMAGE_PARTICIPANT")), nil, 0600)
		deadline := time.Now().Add(30 * time.Second)
		for {
			entries, _ := os.ReadDir(dir)
			if len(entries) >= 2 {
				break
			}
			if time.Now().After(deadline) {
				os.Exit(98)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	call.Start = time.Now().UnixNano()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("CODEFLY_TEST_DISPOSABLE_REAL_DOCKER"), args...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	err := command.Run()
	call.End = time.Now().UnixNano()
	if err != nil {
		call.Error = err.Error()
	}
	file, openErr := os.OpenFile(os.Getenv("CODEFLY_TEST_IMAGE_COMMANDS"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if openErr != nil {
		os.Exit(99)
	}
	_ = json.NewEncoder(file).Encode(call)
	_ = file.Close()
	if err != nil {
		os.Exit(1)
	}
	// Inject an evidence-side host failure AFTER this invocation's own image
	// was loaded; the caller must report failure without rollback/cleanup.
	if build && os.Getenv("CODEFLY_TEST_IMAGE_AFTER_BUILD_ERROR") == "1" {
		fmt.Fprintln(os.Stderr, "intentional failure after owned build")
		os.Exit(96)
	}
	os.Exit(0)
}

func allowedDisposableEngineCall(args []string) bool {
	if len(args) < 2 {
		return false
	}
	if args[0] == "image" && args[1] == "inspect" {
		return true
	}
	if args[0] == "info" && args[1] == "--format" {
		return true
	}
	if args[0] != "buildx" {
		return false
	}
	if args[1] == "inspect" {
		for _, arg := range args[2:] {
			if strings.HasPrefix(arg, "--bootstrap") {
				return false
			}
		}
		return true
	}
	if args[1] != "build" {
		return false
	}
	tags := 0
	for i, arg := range args {
		if strings.HasPrefix(arg, "--push") || strings.HasPrefix(arg, "--cache-to") || strings.HasPrefix(arg, "--output") || strings.HasPrefix(arg, "--tag") {
			return false
		}
		if arg == "-t" {
			if i+1 == len(args) || !strings.HasPrefix(args[i+1], "codefly-local/disposable/") {
				return false
			}
			tags++
		}
	}
	return tags == 1
}

func TestDisposableEngineProxyRefusesForeignEffects(t *testing.T) {
	for _, args := range [][]string{
		{"image", "rm", "anything"}, {"buildx", "create"},
		{"buildx", "build", "-t", "codefly-dev/app/subject:0.0.0"},
		{"buildx", "build", "-t", "codefly-local/disposable/new:one", "-t", "foreign:tag"},
		{"buildx", "build", "-t", "codefly-local/disposable/new:one", "--push=true"},
		{"buildx", "build", "-t", "codefly-local/disposable/new:one", "--output=type=registry"},
	} {
		require.False(t, allowedDisposableEngineCall(args), "%v", args)
	}
	require.True(t, allowedDisposableEngineCall([]string{"buildx", "build", "--load", "-t", "codefly-local/disposable/new:one", "."}))
}

func disposableEngineSetup(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("CODEFLY_TEST_DISPOSABLE_ENGINE") != "1" {
		t.Skip("requires explicit root engine allocation: CODEFLY_TEST_DISPOSABLE_ENGINE=1")
	}
	evidence := os.Getenv("CODEFLY_TEST_DISPOSABLE_EVIDENCE")
	require.True(t, filepath.IsAbs(evidence), "require a new absolute evidence directory")
	require.NoError(t, os.Mkdir(evidence, 0700), "refuse reusing another run's evidence")
	docker, err := exec.LookPath("docker")
	require.NoError(t, err)
	binary, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec \"$CODEFLY_TEST_DOCKER_PROXY\" -test.run=^TestDisposableDockerProxy$ -- \"$@\"\n"), 0700))
	t.Setenv("CODEFLY_TEST_DISPOSABLE_REAL_DOCKER", docker)
	t.Setenv("CODEFLY_TEST_DOCKER_PROXY", binary)
	t.Setenv("CODEFLY_TEST_DOCKER_PROXY_CHILD", "1")
	t.Setenv("CODEFLY_TEST_IMAGE_COMMANDS", filepath.Join(evidence, "engine.jsonl"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return evidence, dir
}

func disposableImageObservation(t *testing.T, ref string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", `{"Id":{{json .Id}},"RepoTags":{{json .RepoTags}},"RepoDigests":{{json .RepoDigests}}}`, ref).CombinedOutput()
	require.NoError(t, err, "inspect %s: %s", ref, out)
	var data struct {
		ID                    string `json:"Id"`
		RepoTags, RepoDigests []string
	}
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(out), &data))
	if expected, ok := disposableProtectedImages[ref]; ok {
		require.Equal(t, expected, data.ID, "protected baseline changed; never restore")
	}
	encoded, err := json.Marshal(data)
	require.NoError(t, err)
	return encoded
}

func TestDisposableImagesEngineQualification(t *testing.T) {
	evidence, proxy := disposableEngineSetup(t)
	baseline := map[string][]byte{}
	for ref := range disposableProtectedImages {
		baseline[ref] = disposableImageObservation(t, ref)
	}
	check := func(label string) {
		for ref, want := range baseline {
			require.Equal(t, want, disposableImageObservation(t, ref), label+": protected reference changed")
		}
		data, err := json.Marshal(baseline)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(evidence, label+"-protected.json"), data, 0600))
	}
	check("preflight")
	t.Cleanup(func() { check("cleanup-observation") })
	protocoltest.Install(t, "disposable-command")
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	t.Setenv("CODEFLY_TEST_PEER_IMAGES", "1")
	t.Setenv("CODEFLY_TEST_PEER_VALIDATION", "stack")
	cli := disposableCommandCLI(t)
	seen := map[string]bool{}
	for _, phase := range []string{"lint", "compile", "test", "build"} {
		before := len(disposableBuildCalls(t, evidence))
		root := disposableCommandWorkspace(t)
		output, err := disposableCommand(t, cli, root, phase).CombinedOutput()
		require.NoError(t, err, "%s: %s", phase, output)
		assertDisposableCommandDelivery(t, root, phase, true)
		require.NoError(t, os.WriteFile(filepath.Join(evidence, phase+".log"), output, 0600))
		for _, call := range disposableBuildCalls(t, evidence)[before:] {
			ref := disposableBuiltRef(call)
			require.False(t, seen[ref], "sequential flows must not reuse destinations")
			seen[ref] = true
		}
		check(phase)
	}
	// Independent process environments and workspaces; no t.Setenv in workers.
	barrier := t.TempDir()
	beforeConcurrent := disposableBuildCalls(t, evidence)
	var commands []*exec.Cmd
	var outputs [2]bytes.Buffer
	for i := 0; i < 2; i++ {
		root := disposableCommandWorkspace(t)
		command := disposableCommand(t, cli, root, "build", "CODEFLY_TEST_IMAGE_BARRIER="+barrier, fmt.Sprintf("CODEFLY_TEST_IMAGE_PARTICIPANT=%d", i))
		command.Stdout, command.Stderr = &outputs[i], &outputs[i]
		if err := command.Start(); err != nil {
			for _, started := range commands {
				_ = started.Wait()
			}
			require.NoError(t, err)
		}
		commands = append(commands, command)
	}
	var waits []error
	for _, command := range commands {
		waits = append(waits, command.Wait())
	}
	for i, err := range waits {
		require.NoError(t, err, "%s", outputs[i].Bytes())
	}
	afterConcurrent := disposableBuildCalls(t, evidence)[len(beforeConcurrent):]
	require.Len(t, afterConcurrent, 2)
	require.Less(t, afterConcurrent[0].Start, afterConcurrent[1].End)
	require.Less(t, afterConcurrent[1].Start, afterConcurrent[0].End, "actual build command intervals must overlap")
	require.NotEqual(t, disposableBuiltRef(afterConcurrent[0]), disposableBuiltRef(afterConcurrent[1]))
	check("concurrent")
	for _, fault := range []string{"CODEFLY_TEST_IMAGE_AFTER_BUILD_ERROR=1", "CODEFLY_TEST_PEER_SBOM_ERROR=1"} {
		root := disposableCommandWorkspace(t)
		before := len(disposableBuildCalls(t, evidence))
		output, err := disposableCommand(t, cli, root, "build", fault).CombinedOutput()
		require.Error(t, err, "fault must fail: %s", output)
		require.Contains(t, string(output), "intentional")
		created := disposableBuildCalls(t, evidence)[before:]
		require.Len(t, created, 1)
		_ = disposableImageObservation(t, disposableBuiltRef(created[0])) // retained after failure
		check(strings.Split(fault, "=")[0])
	}
	// Same-flow reuse through the maintained Builder + actual loopback SBOM
	// peer. Actual engine IDs/platforms are compared independently of replies.
	peer := &disposableRecipePeer{sbomRequests: make(chan *builderv0.SBOMRequest, 2)}
	b, _ := disposableBuilder(t, peer)
	t.Setenv("PATH", proxy+string(os.PathListSeparator)+os.Getenv("PATH"))
	b.world.BuildxBuilder = ""
	b.world.hostArchitecture = nil
	b.world.CollectImageSBOM = true
	beforeReuse := len(disposableBuildCalls(t, evidence))
	var ordinaryRef string
	var ordinaryBefore []byte
	var previous *builderv0.ImageSubject
	for i := 0; i < 2; i++ {
		_, err := b.Plan(t.Context())
		require.NoError(t, err)
		producerPlan := b.planned.response.GetResult().GetDockerBuildPlan()
		original := proto.CloneOf(producerPlan)
		if i == 0 {
			ordinaryRef = disposableOrdinaryRuntimeRef(t, b)
			ordinaryBefore = disposableOptionalObservation(t, ordinaryRef)
			require.NoError(t, os.WriteFile(filepath.Join(evidence, "fixture-ordinary-reference.txt"), []byte(ordinaryRef+"\n"+string(ordinaryBefore)), 0600))
		}
		_, err = b.Build(t.Context())
		require.NoError(t, err)
		require.True(t, proto.Equal(original, producerPlan))
		request := <-peer.sbomRequests
		require.Len(t, request.Subjects, 1)
		subject := request.Subjects[0]
		id, err := inspectLocalImageID(t.Context(), subject.Reference)
		require.NoError(t, err)
		require.Equal(t, id, subject.Digest)
		platform, err := exec.CommandContext(t.Context(), "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", subject.Reference).Output()
		require.NoError(t, err)
		require.Equal(t, strings.TrimSpace(string(platform)), subject.Platform)
		payload, err := protojson.Marshal(request)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(evidence, fmt.Sprintf("sbom-%d.json", i)), payload, 0600))
		require.Contains(t, subject.Reference, "codefly-local/disposable/")
		if previous != nil {
			require.True(t, proto.Equal(previous, subject))
		}
		previous = subject
	}
	require.Equal(t, beforeReuse+1, len(disposableBuildCalls(t, evidence)), "same-flow second SBOM must be a cache hit")
	require.Equal(t, ordinaryBefore, disposableOptionalObservation(t, ordinaryRef))
	check("final")
}

func disposableBuildCalls(t *testing.T, evidence string) []disposableEngineCall {
	t.Helper()
	file, err := os.Open(filepath.Join(evidence, "engine.jsonl"))
	require.NoError(t, err)
	defer file.Close()
	var result []disposableEngineCall
	decoder := json.NewDecoder(file)
	for {
		var call disposableEngineCall
		err = decoder.Decode(&call)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(call.Args) > 1 && call.Args[0] == "buildx" && call.Args[1] == "build" {
			result = append(result, call)
		}
	}
	return result
}

func disposableBuiltRef(call disposableEngineCall) string {
	for i, arg := range call.Args {
		if arg == "-t" && i+1 < len(call.Args) {
			return call.Args[i+1]
		}
	}
	return ""
}

// Derive the ordinary destination through exactly the existing runtime recipe,
// input identity and name selection. This copy never executes Build or writes a
// cache record; the scoped builder remains untouched.
func disposableOrdinaryRuntimeRef(t *testing.T, b *Builder) string {
	t.Helper()
	ordinary := *b
	world := *b.world
	world.disposableImageScope = ""
	world.RebuildImages = true // identity/name only; do not query a cache for reuse
	ordinary.world = &world
	ordinary.localRuntime = true
	plan := b.planned.response.GetResult().GetDockerBuildPlan()
	recipe, err := ordinary.recipeAsBuilt(plan.Recipes[0], false)
	require.NoError(t, err)
	decision, err := ordinary.decideImageReuse(t.Context(), wool.Get(t.Context()), b.planned.outputDir, recipeContextRoot(b.instance.Service.Dir(), b.planned.outputDir, plan), plan, recipe, false)
	require.NoError(t, err)
	require.Regexp(t, `^codefly-local/runtime:sha256-[0-9a-f]{64}$`, decision.image)
	return decision.image
}

func disposableOptionalObservation(t *testing.T, ref string) []byte {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "docker", "image", "inspect", "--format", `{"Id":{{json .Id}},"RepoTags":{{json .RepoTags}},"RepoDigests":{{json .RepoDigests}}}`, ref).CombinedOutput()
	if err != nil {
		require.Contains(t, strings.ToLower(string(output)), "no such image", "ordinary error is not absence")
		return []byte("absent")
	}
	return output
}
