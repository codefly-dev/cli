package list

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func writeWorkspaceWithTwoLibraries(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: test-ws\nlayout: flat\n"), 0o644))

	for name, yaml := range map[string]string{
		"authkit": `kind: library
name: authkit
version: 1.0.0
languages:
  - name: go
    path: go/
    exports:
      - github.com/codefly-dev/authkit-go
`,
		"models": `kind: library
name: models
version: 0.1.0
languages:
  - name: python
    path: python/
`,
	} {
		libDir := filepath.Join(dir, "libraries", name)
		require.NoError(t, os.MkdirAll(libDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(libDir, "library.codefly.yaml"), []byte(yaml), 0o644))
	}
	return dir
}

func newListTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	return cmd, buf
}

func TestLibraryCommandReturnsErrorsThroughCobra(t *testing.T) {
	if LibraryCmd.RunE == nil || LibraryCmd.Run != nil {
		t.Fatal("list libraries command is not exclusively RunE")
	}
	if err := LibraryCmd.Args(LibraryCmd, []string{"extra"}); err == nil {
		t.Fatal("list libraries accepted a positional argument")
	}
}

func TestListLibrariesTableHasARowPerLibrary(t *testing.T) {
	t.Chdir(writeWorkspaceWithTwoLibraries(t))
	t.Cleanup(func() { listLibrariesJSON, listLibrariesRemote = false, false })

	cmd, buf := newListTestCmd()
	require.NoError(t, listLibraries(cmd))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3, "header + two library rows, got:\n%s", buf.String())
	require.Contains(t, lines[0], "NAME")
	require.Contains(t, buf.String(), "authkit")
	require.Contains(t, buf.String(), "models")
}

func TestListLibrariesJSONParses(t *testing.T) {
	t.Chdir(writeWorkspaceWithTwoLibraries(t))
	t.Cleanup(func() { listLibrariesJSON, listLibrariesRemote = false, false })
	listLibrariesJSON = true

	cmd, buf := newListTestCmd()
	require.NoError(t, listLibraries(cmd))

	var entries []libraryListEntry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 2)
	names := []string{entries[0].Name, entries[1].Name}
	require.ElementsMatch(t, []string{"authkit", "models"}, names)
}

func TestListLibrariesMissingWorkspaceReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd, _ := newListTestCmd()
	require.Error(t, listLibraries(cmd))
}

func TestListLibrariesEmptyWorkspace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: empty-ws\nlayout: flat\n"), 0o644))
	t.Chdir(dir)

	cmd, buf := newListTestCmd()
	require.NoError(t, listLibraries(cmd))
	require.Contains(t, buf.String(), "No libraries")
}
