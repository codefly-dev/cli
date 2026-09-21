//go:build darwin && cgo

package composition

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStageBuildRejectsInheritedReadACLBeforeExecution(t *testing.T) {
	session, options, _ := buildStageFixture(t)
	grant := "everyone allow list,search,readattr,readextattr,readsecurity,file_inherit,directory_inherit"
	output, err := exec.CommandContext(t.Context(), "chmod", "+a", grant, options.OutputParent).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() { require.NoError(t, exec.Command("chmod", "-N", options.OutputParent).Run()) })
	result, err := session.StageBuild(t.Context(), options)
	require.ErrorContains(t, err, "must not inherit allow ACLs")
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStageRenderRejectsInheritedReadACLBeforeExecution(t *testing.T) {
	session, files, options := stageFixture(t)
	grant := "everyone allow list,search,readattr,readextattr,readsecurity,file_inherit,directory_inherit"
	output, err := exec.CommandContext(t.Context(), "chmod", "+a", grant, options.OutputParent).CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Cleanup(func() { require.NoError(t, exec.Command("chmod", "-N", options.OutputParent).Run()) })
	result, err := session.StageRender(t.Context(), files, options)
	require.ErrorContains(t, err, "must not inherit allow ACLs")
	require.Nil(t, result)
	entries, err := os.ReadDir(options.OutputParent)
	require.NoError(t, err)
	require.Empty(t, entries)
}
