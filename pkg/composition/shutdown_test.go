package composition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/artifactexecution"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func awaitControlledExecutor(t *testing.T, control string) (*base.TrackedProcessGroup, int) {
	t.Helper()
	var ready struct{ PGID, ChildPID int }
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(control, "ready"))
		return err == nil && json.Unmarshal(data, &ready) == nil
	}, 15*time.Second, 10*time.Millisecond)
	group, err := base.LookupProcessGroup(ready.PGID)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, group.Terminate(ctx))
		require.NoError(t, group.RemoveIfDeadContext(ctx))
	})
	ownership, err := group.InspectOwnership(t.Context())
	require.NoError(t, err)
	require.True(t, ownership.Authenticated)
	if ready.ChildPID != 0 {
		found := false
		for _, member := range ownership.Members {
			found = found || member.PID == ready.ChildPID
		}
		require.True(t, found, "writer must be a real authenticated member, not a detached process")
	}
	return group, ready.ChildPID
}

func releaseControlledExecutor(t *testing.T, control string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(control, "release"), []byte("ready"), 0o600))
}

func TestStageRenderStopsSameGroupWriterBeforeCompletion(t *testing.T) {
	session, files, options := stageFixture(t)
	t.Setenv("HOME", t.TempDir())
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "canceled RPC"}[canceled], func(t *testing.T) {
			control := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
				opts, err := stageTestOptions(directory)
				calls++
				if calls == 1 {
					opts = append(opts, manager.WithEnv("RENDER_TEST_MODE=child-writer", "RENDER_TEST_CONTROL="+control))
				}
				return opts, err
			}
			var result *StagedRender
			var runErr error
			done := make(chan struct{})
			go func() { defer close(done); result, runErr = session.StageRender(ctx, files, options) }()
			group, child := awaitControlledExecutor(t, control)
			require.Positive(t, child)
			if canceled {
				cancel()
			} else {
				releaseControlledExecutor(t, control)
			}
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("render shutdown did not finish")
			}
			require.False(t, group.Alive(), "completion returned with an authenticated writer alive")
			if canceled {
				require.Error(t, runErr)
				require.Nil(t, result)
				require.NotContains(t, runErr.Error(), "shutdown selected renderer")
				return
			}
			require.NoError(t, runErr)
			data, err := os.ReadFile(result.InputsFile)
			require.NoError(t, err)
			var staged DeploymentFiles
			require.NoError(t, json.Unmarshal(data, &staged))
			require.NoError(t, os.WriteFile(filepath.Join(control, "mutate"), []byte("mutate"), 0o600))
			_, err = session.CheckInputs(t.Context(), &staged)
			require.NoError(t, err)
		})
	}
}

func TestInvokeRenderPropagatesShutdownFailureAndDiscardsReceipt(t *testing.T) {
	session, files, options := stageFixture(t)
	t.Setenv("HOME", t.TempDir())
	prepared, err := session.PrepareRender(t.Context(), files)
	require.NoError(t, err)
	acquired, err := session.Acquire(t.Context(), options.HTTPClient)
	require.NoError(t, err)
	var executable string
	for _, artifact := range acquired {
		if artifact.Requirement.Artifact.Name == "renderer" {
			executable = artifact.Path
			break
		}
	}
	require.NotEmpty(t, executable)
	payloads, err := decodeRenderInputs(options.Requests)
	require.NoError(t, err)
	for _, protocol := range []string{artifactexecution.BuilderRender, artifactexecution.SolutionRender} {
		for _, mode := range []string{"success", "wrong-receipt", "canceled"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				var request basev0.ArtifactExecution
				for _, encoded := range prepared {
					require.NoError(t, protojson.Unmarshal(encoded, &request))
					if request.Protocol == protocol {
						break
					}
				}
				var input renderInput
				for _, candidate := range payloads {
					if candidate.target == request.Target && candidate.service == request.Service {
						input = candidate
						break
					}
				}
				require.NotNil(t, input.request)
				directory, pathErr := filepath.EvalSymlinks(t.TempDir())
				require.NoError(t, pathErr)
				control := t.TempDir()
				opts, optionErr := stageTestOptions(directory)
				require.NoError(t, optionErr)
				opts = append(opts, manager.WithEnv("RENDER_TEST_CONTROL="+control, "RENDER_TEST_MODE="+mode))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var receipt *basev0.ArtifactExecutionReceipt
				var runErr error
				done := make(chan struct{})
				go func() {
					defer close(done)
					receipt, runErr = invokeRender(ctx, executable, &request, input.request, directory, opts)
				}()
				group, _ := awaitControlledExecutor(t, control)
				tmp := os.Getenv("TMPDIR")
				require.NoError(t, os.Chmod(tmp, 0o500))
				t.Cleanup(func() { require.NoError(t, os.Chmod(tmp, 0o700)) })
				if mode == "canceled" {
					cancel()
				} else {
					releaseControlledExecutor(t, control)
				}
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Fatal("render shutdown did not finish")
				}
				require.False(t, group.Alive())
				if os.Geteuid() != 0 {
					require.ErrorIs(t, runErr, os.ErrPermission)
					require.ErrorContains(t, runErr, "shutdown selected renderer")
					require.Nil(t, receipt, "even a valid receipt cannot survive failed shutdown")
				} else if mode == "success" {
					require.NoError(t, runErr, "root can remove the read-only snapshot directory")
					require.NotNil(t, receipt)
				}
				if mode != "success" {
					require.ErrorContains(t, runErr, "render failed")
					require.Nil(t, receipt)
				}
				if mode == "canceled" {
					require.NotContains(t, runErr.Error(), "terminate executor process group: context canceled")
				}
			})
		}
	}
}

func TestStageRenderDoesNotPublishCompletionAfterShutdownFailure(t *testing.T) {
	// One selected service ensures a later executor-start failure cannot mask
	// an ignored shutdown error from the successful render.
	session, files, options := stageFixtureWithSelection(t, []string{"left"}, []string{"builder"})
	t.Setenv("HOME", t.TempDir())
	control := t.TempDir()
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		opts, err := stageTestOptions(directory)
		return append(opts, manager.WithEnv("RENDER_TEST_CONTROL="+control)), err
	}
	var result *StagedRender
	var runErr error
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { defer close(done); result, runErr = session.StageRender(ctx, files, options) }()
	group, _ := awaitControlledExecutor(t, control)
	tmp := os.Getenv("TMPDIR")
	require.NoError(t, os.Chmod(tmp, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(tmp, 0o700)) })
	releaseControlledExecutor(t, control)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("stage shutdown did not finish")
	}
	require.False(t, group.Alive())
	if os.Geteuid() == 0 {
		require.NoError(t, runErr)
		return
	}
	require.ErrorIs(t, runErr, os.ErrPermission)
	require.Nil(t, result)
	require.NoError(t, filepath.WalkDir(options.OutputParent, func(path string, _ os.DirEntry, err error) error {
		require.False(t, strings.HasSuffix(path, string(filepath.Separator)+"inputs.json"), "failed shutdown published completion")
		return err
	}))
}
