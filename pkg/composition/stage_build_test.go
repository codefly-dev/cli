package composition

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/artifactexecution"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"gopkg.in/yaml.v3"
)

func buildStageFixture(t *testing.T) (*SelectionSession, *StageOptions, *selectionRegistry) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	tmp, err := os.MkdirTemp("/tmp", "cli-build-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	r := newSelectionRegistry(t)
	certificate := filepath.Join(tmp, "ca.pem")
	require.NoError(t, os.WriteFile(certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.artifacts.Certificate().Raw}), 0o600))
	t.Setenv("SSL_CERT_FILE", certificate)
	binary, _ := executionBinary(t)
	agent := selectionManifest("team/builder", "1.0.0")
	agent.ReleaseArtifacts = []core.ReleaseArtifact{r.artifact("builder", core.ArtifactBuildAgent, binary)}
	tool := r.publish(t, agent)
	module := selectionManifest("team/module", "1.0.0")
	module.AllowDerivedBuilds = true
	module.ReleaseArtifacts = []core.ReleaseArtifact{
		r.artifact("runtime", core.ArtifactRuntime, []byte("released runtime")),
		r.artifact("source", core.ArtifactSource, []byte("selected owner source\n")),
	}
	module.Services = []core.ProvidedService{{Name: "api", AgentUsage: "build", Agent: &core.ComponentDefault{Release: tool, Requirements: map[string]string{"interface": "^1.0.0"}}, RuntimeArtifacts: []string{"runtime"}, ArtifactOperations: []core.ArtifactOperation{
		{Operation: "build", Protocol: artifactexecution.BuilderBuild, Executor: core.ArtifactReference{Target: "services/api/agent", Name: "builder"}, Inputs: map[string]core.ArtifactReference{"source": {Name: "source"}}, Outputs: map[string]string{"runtime": "application/octet-stream"}},
		{Operation: "render", Protocol: artifactexecution.BuilderRender, Executor: core.ArtifactReference{Target: "services/api/agent", Name: "builder"}, Inputs: map[string]core.ArtifactReference{"runtime": {Name: "runtime"}}, Outputs: map[string]string{"manifests": "application/json"}},
	}}}
	release := r.publish(t, module)
	product := selectionManifest("team/product", "1.0.0")
	options := &StageOptions{IdentityKey: bytes.Repeat([]byte{9}, 32), HTTPClient: r.artifacts.Client(), LoadOptions: stageTestOptions}
	for _, name := range []string{"left", "right"} {
		instance := moduleDefault(name, release)
		instance.Services.Include = []string{"api"}
		product.Modules = append(product.Modules, instance)
		options.Requests = append(options.Requests, RenderInput{Target: "modules/" + name, Service: "api", Protocol: artifactexecution.BuilderRender, Request: json.RawMessage(`{"environment":{"name":"test"}}`)})
		options.BuildRequests = append(options.BuildRequests, BuildInput{Target: "modules/" + name, Service: "api", Request: json.RawMessage(`{}`)})
	}
	root := r.publish(t, product)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: []string{"left", "right"}}})
	session.ConfigurationIdentity, err = ExecutionConfigurationIdentity(options.IdentityKey, options.Requests, options.BuildRequests)
	require.NoError(t, err)
	_, err = session.Initialize(t.Context(), &SelectionInputs{Root: root, SourceBuilds: []string{"modules/left", "modules/right"}})
	require.NoError(t, err)
	options.OutputParent, err = filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return session, options, r
}

func TestStageBuildPackagesExactSourcesForIndependentInstances(t *testing.T) {
	session, options, r := buildStageFixture(t)
	before, err := os.ReadFile(filepath.Join(session.Root, SelectionFile))
	require.NoError(t, err)
	result, err := session.StageBuild(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, result.Executions, 2)
	identities := make(map[string]bool)
	for _, execution := range result.Executions {
		var receipt basev0.ArtifactExecutionReceipt
		require.NoError(t, protojson.Unmarshal(execution.Receipt, &receipt))
		require.Len(t, receipt.Outputs, 1)
		require.False(t, identities[receipt.Identity])
		identities[receipt.Identity] = true
		data, err := os.ReadFile(filepath.Join(execution.Directory, receipt.Outputs[0].Path))
		require.NoError(t, err)
		require.Equal(t, receipt.Outputs[0].Digest, contentDigest(data))
		reader, err := gzip.NewReader(bytes.NewReader(data))
		require.NoError(t, err)
		source, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.Equal(t, "selected owner source\n", string(source))
	}
	completed, err := os.ReadFile(result.EvidenceFile)
	require.NoError(t, err)
	require.NotContains(t, string(completed), "selected owner source")
	after, err := os.ReadFile(filepath.Join(session.Root, SelectionFile))
	require.NoError(t, err)
	require.Equal(t, before, after)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, path := range r.requests {
		require.NotContains(t, path, "module.tar")
	}
}

func TestStageBuildRefusesFailuresAndPreservesCompletedBatch(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	completed, err := session.StageBuild(t.Context(), options)
	require.NoError(t, err)
	for _, mode := range []string{"missing-protocol", "unsupported", "failed", "missing-receipt", "wrong-receipt", "extra-file"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RENDER_TEST_MODE", mode)
			result, err := session.StageBuild(t.Context(), options)
			require.Error(t, err)
			require.Nil(t, result)
			entries, err := os.ReadDir(options.OutputParent)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, filepath.Base(completed.Directory), entries[0].Name())
		})
	}
}

func TestStageBuildCancellationCleansAndReleasesSelectionLock(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	t.Setenv("RENDER_TEST_MODE", "wait")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result, err := session.StageBuild(ctx, options)
	require.Error(t, err)
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
	_, err = session.Inspect(t.Context())
	require.NoError(t, err)
}

func TestStageBuildCommandRunsRealSelectedBuilder(t *testing.T) {
	session, options, registry := buildStageFixture(t)
	directory := t.TempDir()
	paths := make(map[string]string)
	for name, value := range map[string]any{"render": options.Requests, "build": options.BuildRequests} {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		paths[name] = filepath.Join(directory, name+".json")
		require.NoError(t, os.WriteFile(paths[name], data, 0o600))
	}
	keyPath := filepath.Join(directory, "key")
	require.NoError(t, os.WriteFile(keyPath, options.IdentityKey, 0o600))
	command := exec.CommandContext(t.Context(), "go", "run", "../../cmd/codefly", "composition",
		"--workspace", registry.workspace, "--product", session.Root, "--identity-key", keyPath,
		"--render-requests", paths["render"], "--build-requests", paths["build"],
		"stage-build", "--output-parent", options.OutputParent, "--sandbox", "none", "--without-principal")
	command.Env = append(os.Environ(), "GOWORK=off")
	var narration bytes.Buffer
	command.Stderr = &narration
	data, err := command.Output()
	require.NoError(t, err, "%s\n%s", data, narration.String())
	var result StagedBuild
	require.NoError(t, json.Unmarshal(data, &result), "%s", data)
	require.Len(t, result.Executions, 2)
	require.FileExists(t, result.EvidenceFile)
}

func TestStageBuildRejectsSourceAndPayloadDrift(t *testing.T) {
	session, options, registry := buildStageFixture(t)
	original := options.BuildRequests[0].Request
	options.BuildRequests[0].Request = json.RawMessage(`{"buildContext":{}}`)
	result, err := session.StageBuild(t.Context(), options)
	require.ErrorContains(t, err, "configuration differs")
	require.Nil(t, result)
	options.BuildRequests[0].Request = original
	registry.mu.Lock()
	for path, data := range registry.content {
		if string(data) == "selected owner source\n" {
			registry.content[path] = []byte("private patch")
		}
	}
	registry.mu.Unlock()
	result, err = session.StageBuild(t.Context(), options)
	require.ErrorContains(t, err, "build failed")
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStageBuildRechecksEarlierOutputsBeforeCompletion(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		if filepath.Base(directory) == "0001" {
			if err := os.WriteFile(filepath.Join(filepath.Dir(directory), "0000", "runtime.json"), []byte("changed while later build starts"), 0o600); err != nil {
				return nil, err
			}
		}
		return stageTestOptions(directory)
	}
	result, err := session.StageBuild(t.Context(), options)
	require.Error(t, err)
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStageBuildStopsRegisteredChildWriter(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	control := t.TempDir()
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		opts, err := stageTestOptions(directory)
		if filepath.Base(directory) == "0000" {
			opts = append(opts, manager.WithEnv("RENDER_TEST_MODE=child-writer", "RENDER_TEST_CONTROL="+control))
		}
		return opts, err
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var result *StagedBuild
	var runErr error
	done := make(chan struct{})
	go func() { defer close(done); result, runErr = session.StageBuild(ctx, options) }()
	group, child := awaitControlledExecutor(t, control)
	require.Positive(t, child)
	releaseControlledExecutor(t, control)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("build failed to finish registered-group shutdown")
	}
	require.NoError(t, runErr)
	require.NotNil(t, result)
	require.False(t, group.Alive(), "a completed build must not retain its registered writer")
}

func TestStageBuildRejectsShutdownCleanupFailure(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	t.Setenv("HOME", t.TempDir())
	control := t.TempDir()
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		opts, err := stageTestOptions(directory)
		if filepath.Base(directory) == "0000" {
			opts = append(opts, manager.WithEnv("RENDER_TEST_CONTROL="+control))
		}
		return opts, err
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var result *StagedBuild
	var runErr error
	done := make(chan struct{})
	go func() { defer close(done); result, runErr = session.StageBuild(ctx, options) }()
	group, _ := awaitControlledExecutor(t, control)
	tmp := os.Getenv("TMPDIR")
	require.NoError(t, os.Chmod(tmp, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(tmp, 0o700)) })
	releaseControlledExecutor(t, control)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("build shutdown did not finish")
	}
	require.False(t, group.Alive())
	if os.Geteuid() == 0 {
		require.NoError(t, runErr, "root can remove the protected snapshot")
		return
	}
	require.ErrorIs(t, runErr, os.ErrPermission)
	require.ErrorContains(t, runErr, "shutdown selected builder")
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStageBuildRejectsReplaceableOutputAncestry(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	require.NoError(t, os.Chmod(options.OutputParent, 0o777))
	t.Cleanup(func() { require.NoError(t, os.Chmod(options.OutputParent, 0o700)) })
	result, err := session.StageBuild(t.Context(), options)
	require.ErrorContains(t, err, "must not be group/world writable")
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStagedBuildRequiresOwnerAuthorizedDerivedEvidenceBeforeRender(t *testing.T) {
	session, options, registry := buildStageFixture(t)
	// Install real package-scoped build authority in the workspace, then reopen
	// through the normal trust loader. A build receipt cannot authorize itself.
	workspacePath := filepath.Join(registry.workspace, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	var workspace rawWorkspaceModuleTrustProbe
	require.NoError(t, yaml.Unmarshal(data, &workspace))
	workspace.ModuleTrust.BuildSigners = map[string]map[string]string{"team/module": {"packager": base64.StdEncoding.EncodeToString(registry.key.Public().(ed25519.PublicKey))}}
	data, err = yaml.Marshal(map[string]any{"name": "product", "module-trust": workspace.ModuleTrust})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(workspacePath, data, 0o600))
	session, err = NewSelectionSession(registry.workspace, session.Root, session.ConfigurationIdentity)
	require.NoError(t, err)
	built, err := session.StageBuild(t.Context(), options)
	require.NoError(t, err)
	files := &DeploymentFiles{Bindings: map[string]string{"target": contentDigest([]byte("test target"))}}
	for _, execution := range built.Executions {
		var receipt basev0.ArtifactExecutionReceipt
		require.NoError(t, protojson.Unmarshal(execution.Receipt, &receipt))
		output := receipt.Outputs[0]
		path := filepath.Join(execution.Directory, output.Path)
		outputBytes, err := os.ReadFile(path)
		require.NoError(t, err)
		// Real HTTPS test-origin publication, not a made-up artifact URL.
		artifact := registry.artifact("runtime", core.ArtifactRuntime, outputBytes)
		statement, err := json.Marshal(core.DerivedOutput{Schema: "codefly/derived-output/v1",
			SelectionIdentity: built.SelectionIdentity, Target: execution.Target, Artifact: "runtime",
			SourceIdentity: contentDigest([]byte("selected owner source\n")), ExecutionIdentity: receipt.Identity,
			Digest: artifact.Digest, URI: artifact.URI, Signer: "packager"})
		require.NoError(t, err)
		files.Derived = append(files.Derived, core.SignedDerivedOutput{Statement: statement, Signature: ed25519.Sign(registry.key, statement)})
		files.Runtime = append(files.Runtime, RuntimeFile{Target: execution.Target, Name: "runtime", Path: path})
	}
	derived := files.Derived
	files.Derived = nil
	_, err = session.StageRender(t.Context(), files, options)
	require.ErrorContains(t, err, "authorized derived output")
	files.Derived = derived
	rendered, err := session.StageRender(t.Context(), files, options)
	require.NoError(t, err)
	require.Equal(t, built.SelectionIdentity, rendered.Record.SelectionIdentity)
	require.NotEmpty(t, rendered.Record.ExecutionIdentity)
	options.BuildRequests = nil
	_, err = session.StageRender(t.Context(), files, options)
	require.ErrorContains(t, err, "configuration differs")
}

func TestStageBuildConcurrentBatchesStayIndependent(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	var results [2]*StagedBuild
	var failures [2]error
	var workers sync.WaitGroup
	for i := range results {
		workers.Go(func() { results[i], failures[i] = session.StageBuild(t.Context(), options) })
	}
	workers.Wait()
	for i := range results {
		require.NoError(t, failures[i])
		require.FileExists(t, results[i].EvidenceFile)
	}
	require.NotEqual(t, results[0].Directory, results[1].Directory)
	require.Equal(t, results[0].SelectionIdentity, results[1].SelectionIdentity)
}

func TestStageBuildRejectsMissingAndUnselectedPayloadsBeforeExecution(t *testing.T) {
	for _, mode := range []string{"missing", "unselected"} {
		t.Run(mode, func(t *testing.T) {
			session, options, _ := buildStageFixture(t)
			if mode == "missing" {
				options.BuildRequests = options.BuildRequests[:1]
			} else {
				options.BuildRequests[0].Target = "modules/unselected"
			}
			var err error
			session.ConfigurationIdentity, err = ExecutionConfigurationIdentity(options.IdentityKey, options.Requests, options.BuildRequests)
			require.NoError(t, err)
			result, err := session.StageBuild(t.Context(), options)
			require.ErrorContains(t, err, "payload")
			require.Nil(t, result)
			entries, err := os.ReadDir(options.OutputParent)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}
