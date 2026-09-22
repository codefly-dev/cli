package composition

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/artifactexecution"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

type renderExecutor struct {
	data []byte
	path string
}

// The executor lives outside TMPDIR, which fixtures redirect at a per-test
// directory they delete on cleanup, and is built from the environment as the
// process received it: fixtures install an httptest CA over SSL_CERT_FILE
// before they ask for it, which would reach a module fetch.
var (
	renderExecutorDirectory   string
	renderExecutorEnvironment []string
	renderExecutorBuilds      atomic.Int64
)

func TestMain(m *testing.M) {
	renderExecutorEnvironment = append(os.Environ(), "GOWORK=off")
	code := m.Run()
	if renderExecutorDirectory != "" {
		_ = os.RemoveAll(renderExecutorDirectory)
	}
	os.Exit(code)
}

// Nearly every fixture here needs this executable, and the `go build` that
// produces it cost more than every assertion in the package combined when each
// test ran its own, so the whole test binary shares one build. The directory
// is created on demand rather than in TestMain because this package re-execs
// its own test binary, and those children exit without reaching any cleanup.
var buildRenderExecutor = sync.OnceValues(func() (renderExecutor, error) {
	renderExecutorBuilds.Add(1)
	directory, err := os.MkdirTemp("/tmp", "cli-renderexecutor-")
	if err != nil {
		return renderExecutor{}, err
	}
	renderExecutorDirectory = directory
	binary := filepath.Join(directory, "renderer")
	command := exec.Command("go", "build", "-o", binary, "./testdata/renderexecutor")
	command.Env = renderExecutorEnvironment
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		return renderExecutor{}, fmt.Errorf("%w: %s", buildErr, output)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return renderExecutor{}, err
	}
	return renderExecutor{data: data, path: binary}, nil
})

func executionBinary(t *testing.T) ([]byte, string) {
	t.Helper()
	executor, err := buildRenderExecutor()
	require.NoError(t, err)
	return executor.data, executor.path
}

func TestRenderExecutorIsCompiledOncePerRun(t *testing.T) {
	_, first := executionBinary(t)
	_, second := executionBinary(t)
	require.Equal(t, first, second)
	require.Equal(t, int64(1), renderExecutorBuilds.Load(), "every fixture must share one compilation")
}

func executionClient(t *testing.T, binary, capability string) *services.BuilderAgent {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	addressFile := filepath.Join(t.TempDir(), "address")
	command := exec.CommandContext(ctx, binary, "--address-file", addressFile, "--contract", capability)
	require.NoError(t, command.Start())
	t.Cleanup(func() { cancel(); _ = command.Wait() })
	var address []byte
	require.Eventually(t, func() bool {
		var err error
		address, err = os.ReadFile(addressFile)
		return err == nil && len(address) > 0
	}, 10*time.Second, 10*time.Millisecond)
	connection, err := grpc.NewClient(string(address), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	data, err := os.ReadFile(binary)
	require.NoError(t, err)
	client := services.NewBuilderAgentClient(connection)
	client.ProcessInfo = &manager.ProcessInfo{PID: command.Process.Pid, ArtifactDigest: contentDigest(data)}
	return client
}

func declareRender(manifest *core.PackageManifest, service string) {
	manifest.Services = append(manifest.Services, core.ProvidedService{Name: service, RuntimeArtifacts: []string{"runtime"},
		ArtifactOperations: []core.ArtifactOperation{{
			Operation: "render", Protocol: artifactexecution.BuilderRender,
			Executor: core.ArtifactReference{Name: "renderer"},
			Inputs:   map[string]core.ArtifactReference{"application": {Name: "runtime"}},
			Outputs:  map[string]string{"manifests": "application/json"},
		}},
	})
}

func renderFiles(t *testing.T, session *SelectionSession, files *DeploymentFiles, client *services.BuilderAgent) {
	t.Helper()
	requests, err := session.PrepareRender(t.Context(), files)
	require.NoError(t, err)
	files.Executions = nil
	for _, data := range requests {
		require.NotContains(t, string(data), "not-for-output")
		var request basev0.ArtifactExecution
		require.NoError(t, protojson.Unmarshal(data, &request))
		directory, err := filepath.EvalSymlinks(t.TempDir())
		require.NoError(t, err)
		response, err := client.Deploy(t.Context(), &builderv0.DeploymentRequest{Execution: &request, OutputDirectory: directory})
		require.NoError(t, err)
		receipt, err := protojson.Marshal(response.Execution)
		require.NoError(t, err)
		files.Executions = append(files.Executions, ExecutionFiles{Target: request.Target, Service: request.Service, Directory: directory, Receipt: receipt})
	}
}

func TestStagedExecutionBindsEveryServiceAndRejectsChangedEvidence(t *testing.T) {
	r := newSelectionRegistry(t)
	binary, path := executionBinary(t)
	manifest := selectionManifest("team/foo", "1.0.0")
	manifest.ReleaseArtifacts = []core.ReleaseArtifact{
		r.artifact("runtime", core.ArtifactRuntime, []byte("runtime")),
		r.artifact("renderer", core.ArtifactBuildAgent, binary),
	}
	declareRender(manifest, "first")
	declareRender(manifest, "second")
	module := r.publish(t, manifest)
	product := selectionManifest("team/product", "1.0.0")
	for _, name := range []string{"left", "right"} {
		instance := moduleDefault(name, module)
		instance.Services.Include = []string{"first", "second"}
		product.Modules = append(product.Modules, instance)
	}
	root := r.publish(t, product)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"left", "right"}}})
	_, err := session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	acquired, err := session.Acquire(t.Context(), r.artifacts.Client())
	require.NoError(t, err)
	files := &DeploymentFiles{Bindings: map[string]string{"target": contentDigest([]byte("cluster"))}}
	for _, artifact := range acquired {
		if artifact.Requirement.Artifact.Name == "runtime" {
			files.Runtime = append(files.Runtime, RuntimeFile{Target: artifact.Requirement.Target, Name: "runtime", Path: artifact.Path})
		}
	}
	client := executionClient(t, path, artifactexecution.Contract)
	renderFiles(t, session, files, client)
	require.Len(t, files.Executions, 4)
	require.NotEqual(t, files.Executions[0].Target, files.Executions[2].Target)
	record, err := session.CheckInputs(t.Context(), files)
	require.NoError(t, err)
	require.NotEmpty(t, record.ExecutionIdentity)
	require.Len(t, record.Executions, 4)
	original := append([]ExecutionFiles(nil), files.Executions...)
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"missing", func() { files.Executions = files.Executions[:1] }},
		{"duplicate", func() { files.Executions[1] = files.Executions[0] }},
		{"foreign service", func() { files.Executions[0].Service = "other" }},
		{"unknown receipt field", func() { files.Executions[0].Receipt = []byte(`{"unknown":true}`) }},
		{"shared directory", func() { files.Executions[1].Directory = files.Executions[0].Directory }},
		{"relative directory", func() { files.Executions[0].Directory = "." }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files.Executions = append([]ExecutionFiles(nil), original...)
			tc.change()
			_, err := session.CheckInputs(t.Context(), files)
			require.Error(t, err)
		})
	}
	files.Executions = original
	output := filepath.Join(original[0].Directory, "manifests.json")
	before, err := os.ReadFile(output)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(output, []byte("changed output"), 0o600))
	_, err = session.CheckInputs(t.Context(), files)
	require.ErrorIs(t, err, core.ErrDigestMismatch)
	require.NoError(t, os.WriteFile(output, before, 0o600))
	extra := filepath.Join(original[0].Directory, "unreceipted")
	require.NoError(t, os.WriteFile(extra, []byte("extra"), 0o600))
	_, err = session.CheckInputs(t.Context(), files)
	require.ErrorContains(t, err, "undeclared")
	require.NoError(t, os.Remove(extra))
	require.NoError(t, os.Remove(output))
	require.NoError(t, os.Symlink(files.Runtime[0].Path, output))
	_, err = session.CheckInputs(t.Context(), files)
	require.ErrorContains(t, err, "nonregular")

	requests, err := session.PrepareRender(t.Context(), files)
	require.NoError(t, err)
	var request basev0.ArtifactExecution
	require.NoError(t, protojson.Unmarshal(requests[0], &request))
	unsupported := executionClient(t, path, "artifact-execution/v2")
	directory := t.TempDir()
	_, err = unsupported.Deploy(t.Context(), &builderv0.DeploymentRequest{Execution: &request, OutputDirectory: directory})
	require.ErrorContains(t, err, "does not advertise")
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestExecutionDirectoriesMustBeDisjointAndCanonical(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	child := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(child, 0o700))
	for _, pair := range [][2]string{{root, root}, {root, child}, {child, root}} {
		_, err := isolatedExecutionDirectory(pair[0], []string{pair[1]})
		require.ErrorContains(t, err, "disjoint")
	}
	link := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(child, link))
	_, err = isolatedExecutionDirectory(link, nil)
	require.ErrorContains(t, err, "symlinks")
}
