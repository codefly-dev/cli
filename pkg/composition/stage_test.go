package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/artifactexecution"
	core "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func stageFixture(t *testing.T) (*SelectionSession, *DeploymentFiles, *StageOptions) {
	t.Helper()
	return stageFixtureWithSelection(t, []string{"left", "right"}, []string{"builder", "solution"})
}

func stageFixtureWithSelection(t *testing.T, targets, services []string) (*SelectionSession, *DeploymentFiles, *StageOptions) {
	t.Helper()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	// Keep real process sockets below the Unix-domain path length limit.
	tmp, err := os.MkdirTemp("/tmp", "cli-render-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	r := newSelectionRegistry(t)
	binary, _ := executionBinary(t)
	manifest := selectionManifest("team/module", "1.0.0")
	manifest.ReleaseArtifacts = []core.ReleaseArtifact{
		r.artifact("runtime", core.ArtifactRuntime, []byte("runtime")),
		r.artifact("renderer", core.ArtifactBuildAgent, binary),
	}
	declareRender(manifest, "builder")
	declareRender(manifest, "solution")
	manifest.Services[1].ArtifactOperations[0].Protocol = artifactexecution.SolutionRender
	module := r.publish(t, manifest)
	product := selectionManifest("team/product", "1.0.0")
	var requests []RenderInput
	for _, target := range targets {
		instance := moduleDefault(target, module)
		instance.Services.Include = services
		product.Modules = append(product.Modules, instance)
		for _, input := range []RenderInput{
			{Target: "modules/" + target, Service: "builder", Protocol: artifactexecution.BuilderRender, Request: json.RawMessage(`{"environment":{"name":"test"}}`)},
			{Target: "modules/" + target, Service: "solution", Protocol: artifactexecution.SolutionRender, Request: json.RawMessage(`{"values":{"secret":"not-for-output"}}`)},
		} {
			if slices.Contains(services, input.Service) {
				requests = append(requests, input)
			}
		}
	}
	root := r.publish(t, product)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: root.ID, Version: root.Version}, Modules: core.ModuleInstances{Include: targets}})
	key := bytes.Repeat([]byte{9}, 32)
	session.ConfigurationIdentity, err = RenderConfigurationIdentity(key, requests)
	require.NoError(t, err)
	_, err = session.Initialize(t.Context(), &SelectionInputs{Root: root})
	require.NoError(t, err)
	acquired, err := session.Acquire(t.Context(), r.artifacts.Client())
	require.NoError(t, err)
	files := &DeploymentFiles{Bindings: map[string]string{"target": contentDigest([]byte("cluster"))}}
	for _, artifact := range acquired {
		if artifact.Requirement.Artifact.Name == "runtime" {
			files.Runtime = append(files.Runtime, RuntimeFile{Target: artifact.Requirement.Target, Name: "runtime", Path: artifact.Path})
		}
		info, statErr := os.Stat(artifact.Path)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return session, files, &StageOptions{OutputParent: parent, Requests: requests, IdentityKey: key, HTTPClient: r.artifacts.Client(), LoadOptions: stageTestOptions}
}

func stageTestOptions(directory string) ([]manager.LoadOption, error) {
	return []manager.LoadOption{manager.WithoutSandbox(), manager.WithoutPrincipal(), manager.WithUDS(), manager.WithWorkDir(directory)}, nil
}

func TestStageRenderAuthenticatesBothProtocolsAndPreservesInputs(t *testing.T) {
	session, files, options := stageFixture(t)
	installed := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "renderer", Version: "1.0.0"}
	installPath, err := installed.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(installPath), 0o700))
	require.NoError(t, os.WriteFile(installPath, []byte("existing installation"), 0o600))
	before, err := json.Marshal(files)
	require.NoError(t, err)
	result, err := session.StageRender(t.Context(), files, options)
	require.NoError(t, err)
	require.Len(t, result.Record.Executions, 4)
	require.NotEmpty(t, result.Record.ExecutionIdentity)
	data, err := os.ReadFile(result.InputsFile)
	require.NoError(t, err)
	require.NotContains(t, string(data), "not-for-output")
	var staged DeploymentFiles
	require.NoError(t, json.Unmarshal(data, &staged))
	record, err := session.CheckInputs(t.Context(), &staged)
	require.NoError(t, err)
	require.Equal(t, result.Record.ExecutionIdentity, record.ExecutionIdentity)
	after, err := json.Marshal(files)
	require.NoError(t, err)
	require.Equal(t, before, after)
	installedBytes, err := os.ReadFile(installPath)
	require.NoError(t, err)
	require.Equal(t, "existing installation", string(installedBytes))
	_, err = session.Admit(t.Context(), &staged, core.DeploymentPolicy{}, time.Now())
	require.Error(t, err, "staging never supplies approval")
	info, err := os.Stat(result.InputsFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	output := filepath.Join(staged.Executions[0].Directory, "manifests.json")
	require.NoError(t, os.WriteFile(output, []byte("changed"), 0o600))
	_, err = session.CheckInputs(t.Context(), &staged)
	require.ErrorIs(t, err, core.ErrDigestMismatch)
}

func TestStageRenderFailureRemovesOnlyItsIncompleteBatch(t *testing.T) {
	session, files, options := stageFixture(t)
	retained := filepath.Join(options.OutputParent, "retained")
	require.NoError(t, os.WriteFile(retained, []byte("keep"), 0o600))
	for _, mode := range []string{"missing-protocol", "unsupported", "missing-receipt", "wrong-receipt", "extra-file", "failed"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
				calls++
				opts, err := stageTestOptions(directory)
				// Fail only after an earlier member has rendered successfully.
				if calls > 1 {
					opts = append(opts, manager.WithEnv("RENDER_TEST_MODE="+mode))
				}
				return opts, err
			}
			result, err := session.StageRender(t.Context(), files, options)
			require.Error(t, err)
			require.Nil(t, result)
			require.GreaterOrEqual(t, calls, 2)
			entries, err := os.ReadDir(options.OutputParent)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, "retained", entries[0].Name())
		})
	}
	t.Run("cleanup denial is reported", func(t *testing.T) {
		var directory string
		options.LoadOptions = func(output string) ([]manager.LoadOption, error) {
			directory = filepath.Dir(output)
			if err := os.Chmod(directory, 0o500); err != nil {
				return nil, err
			}
			return nil, errors.New("policy rejected")
		}
		t.Cleanup(func() {
			if directory != "" {
				_ = os.Chmod(directory, 0o700)
				_ = os.RemoveAll(directory)
			}
		})
		result, err := session.StageRender(t.Context(), files, options)
		require.Nil(t, result)
		require.ErrorContains(t, err, "policy rejected")
		if os.Geteuid() == 0 {
			// Root can remove the read-only directory; it must not invent a cleanup error.
			require.NotContains(t, err.Error(), "remove incomplete staging")
		} else {
			require.ErrorContains(t, err, "remove incomplete staging")
		}
		require.NoFileExists(t, filepath.Join(directory, "inputs.json"))
	})
}

func TestStageRenderCancellationAndConcurrentBatches(t *testing.T) {
	session, files, options := stageFixture(t)
	t.Run("cancel active executor", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waiting := *options
		started := make(chan string, 1)
		waiting.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
			opts, err := stageTestOptions(directory)
			started <- directory
			return append(opts, manager.WithEnv("RENDER_TEST_MODE=wait")), err
		}
		var result *StagedRender
		var err error
		done := make(chan struct{})
		go func() { defer close(done); result, err = session.StageRender(ctx, files, &waiting) }()
		select {
		case directory := <-started:
			require.Eventually(t, func() bool { _, statErr := os.Stat(filepath.Join(directory, "waiting")); return statErr == nil }, 10*time.Second, 10*time.Millisecond)
		case <-done:
			t.Fatalf("staging ended before executor start: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("executor start timed out")
		}
		cancel()
		<-done
		require.Error(t, err)
		require.Nil(t, result)
		entries, err := os.ReadDir(options.OutputParent)
		require.NoError(t, err)
		require.Empty(t, entries)
	})
	t.Run("independent batches", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make([]*StagedRender, 2)
		errors := make([]error, 2)
		for i := range results {
			wg.Go(func() { results[i], errors[i] = session.StageRender(t.Context(), files, options) })
		}
		wg.Wait()
		for i := range results {
			require.NoError(t, errors[i])
			require.FileExists(t, results[i].InputsFile)
		}
		require.NotEqual(t, results[0].Directory, results[1].Directory)
		require.Equal(t, results[0].Record.ExecutionIdentity, results[1].Record.ExecutionIdentity)
	})
}

func TestStageRenderRejectsChangedPayloadAndSourceDirectories(t *testing.T) {
	session, files, options := stageFixture(t)
	changed := *options
	changed.Requests = append([]RenderInput(nil), options.Requests...)
	changed.Requests[0].Request = json.RawMessage(`{"environment":{"name":"other"}}`)
	_, err := session.StageRender(t.Context(), files, &changed)
	require.ErrorContains(t, err, "configuration differs")
	changed = *options
	changed.OutputParent, err = filepath.EvalSymlinks(session.Root)
	require.NoError(t, err)
	_, err = session.StageRender(t.Context(), files, &changed)
	require.ErrorContains(t, err, "outside the product")
	local, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.ErrorContains(t, session.checkStagingParent(local, map[string]string{"left": local}), "outside the product")
	changed = *options
	changed.LoadOptions = nil
	_, err = session.StageRender(t.Context(), files, &changed)
	require.ErrorContains(t, err, "explicit executor")
	qualified := *files
	qualified.Qualifications = []core.SignedQualification{{}}
	_, err = session.StageRender(t.Context(), &qualified, options)
	require.ErrorContains(t, err, "previous execution evidence or qualifications")
}
