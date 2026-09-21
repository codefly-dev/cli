package composition

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func localBuildCheckouts(t *testing.T, session *SelectionSession, registry *selectionRegistry) map[string]string {
	t.Helper()
	registry.mu.Lock()
	manifest := bytes.Clone(registry.assets[registry.releases["team/module@v1.0.0"][core.PackageManifestFileName]])
	registry.mu.Unlock()
	checkouts := make(map[string]string)
	for _, name := range []string{"left", "right"} {
		directory := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(directory, "runtime"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, core.PackageManifestFileName), manifest, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "runtime/source.txt"), []byte(name+" local source\n"), 0o600))
		output, err := exec.CommandContext(t.Context(), "git", "init", directory).CombinedOutput()
		require.NoError(t, err, string(output))
		checkouts["modules/"+name] = directory
	}
	inspection, err := session.Develop(t.Context(), checkouts)
	require.NoError(t, err)
	for target := range checkouts {
		require.NotNil(t, inspection.Checkouts[target].Dirty)
		require.True(t, *inspection.Checkouts[target].Dirty)
	}
	return checkouts
}

func TestStageBuildIndependentLocalCheckoutsPreserveReleases(t *testing.T) {
	session, options, registry := buildStageFixture(t)
	released, err := session.Inspect(t.Context())
	require.NoError(t, err)
	before, err := os.ReadFile(filepath.Join(session.Root, SelectionFile))
	require.NoError(t, err)
	checkouts := localBuildCheckouts(t, session, registry)
	built, err := session.StageBuild(t.Context(), options)
	require.NoError(t, err)
	require.Len(t, built.Executions, 2)
	for _, execution := range built.Executions {
		var receipt basev0.ArtifactExecutionReceipt
		require.NoError(t, protojson.Unmarshal(execution.Receipt, &receipt))
		data, readErr := os.ReadFile(filepath.Join(execution.Directory, receipt.Outputs[0].Path))
		require.NoError(t, readErr)
		reader, openErr := gzip.NewReader(bytes.NewReader(data))
		require.NoError(t, openErr)
		actual, readErr := io.ReadAll(reader)
		require.NoError(t, readErr)
		require.NoError(t, reader.Close())
		expected, readErr := os.ReadFile(filepath.Join(checkouts[execution.Target], "runtime/source.txt"))
		require.NoError(t, readErr)
		require.Equal(t, expected, actual)
	}
	_, err = session.CheckInputs(t.Context(), &DeploymentFiles{})
	require.ErrorContains(t, err, "local development substitutions")
	changedPath := filepath.Join(checkouts["modules/left"], "runtime/source.txt")
	require.NoError(t, os.WriteFile(changedPath, []byte("edited local source"), 0o600))
	edited, err := session.StageBuild(t.Context(), options)
	require.NoError(t, err)
	require.NotEqual(t, built.SelectionIdentity, edited.SelectionIdentity)
	require.NotEqual(t, built.Identity, edited.Identity)
	restored, err := session.Develop(t.Context(), map[string]string{"modules/left": "", "modules/right": ""})
	require.NoError(t, err)
	require.Equal(t, released.Identity, restored.Identity)
	actual, err := os.ReadFile(changedPath)
	require.NoError(t, err)
	require.Equal(t, "edited local source", string(actual))
	after, err := os.ReadFile(filepath.Join(session.Root, SelectionFile))
	require.NoError(t, err)
	require.Equal(t, before, after)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, request := range registry.requests {
		require.NotContains(t, request, contentDigest([]byte("selected owner source\n"))[len("sha256:"):], "local build must not download released source")
	}
}

func TestStageBuildRejectsLocalChangesDuringInvocation(t *testing.T) {
	session, options, registry := buildStageFixture(t)
	checkouts := localBuildCheckouts(t, session, registry)
	options.LoadOptions = func(directory string) ([]manager.LoadOption, error) {
		if filepath.Base(directory) == "0000" {
			if err := os.WriteFile(filepath.Join(checkouts["modules/left"], "runtime/source.txt"), []byte("edited after preparation"), 0o600); err != nil {
				return nil, err
			}
		}
		return stageTestOptions(directory)
	}
	built, err := session.StageBuild(t.Context(), options)
	require.ErrorIs(t, err, core.ErrDigestMismatch)
	require.Nil(t, built)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
	_, err = session.Inspect(t.Context())
	require.NoError(t, err, "rejected build must release the selection lock")
	actual, err := os.ReadFile(filepath.Join(checkouts["modules/left"], "runtime/source.txt"))
	require.NoError(t, err)
	require.Equal(t, "edited after preparation", string(actual), "cleanup must not restore or remove local edits")
}
