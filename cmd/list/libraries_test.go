package list

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestListLibrariesRemoteWithoutStoreConfigStillListsLibraries reproduces the
// state every workspace starts in the moment this feature ships: no
// libraries.publish block configured yet. --remote must still list every
// local library instead of aborting on the first language it can't resolve a
// store for.
func TestListLibrariesRemoteWithoutStoreConfigStillListsLibraries(t *testing.T) {
	t.Chdir(writeWorkspaceWithTwoLibraries(t))
	t.Cleanup(func() { listLibrariesJSON, listLibrariesRemote = false, false })
	listLibrariesRemote = true

	cmd, buf := newListTestCmd()
	require.NoError(t, listLibraries(cmd), "a workspace with no libraries.publish config must still list its local libraries")
	require.Contains(t, buf.String(), "authkit")
	require.Contains(t, buf.String(), "models")
}

// TestListLibrariesRemoteContinuesPastAPerLibraryStoreFailure proves a single
// library's remote lookup failure (here, its store returning a hard error —
// the same shape as a repository that has never been published) does not
// hide every other library from the listing.
func TestListLibrariesRemoteContinuesPastAPerLibraryStoreFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "broken") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	workspaceYAML := fmt.Sprintf(`name: test-ws
layout: flat
libraries:
  publish:
    typescript: {registry: %q, scope: "@codefly-dev"}
`, server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(workspaceYAML), 0o644))
	for _, name := range []string{"working", "broken"} {
		libDir := filepath.Join(dir, "libraries", name)
		require.NoError(t, os.MkdirAll(libDir, 0o755))
		yaml := fmt.Sprintf("kind: library\nname: %s\nversion: 1.0.0\nlanguages:\n  - name: typescript\n    path: ts/\n", name)
		require.NoError(t, os.WriteFile(filepath.Join(libDir, "library.codefly.yaml"), []byte(yaml), 0o644))
	}
	t.Chdir(dir)
	t.Cleanup(func() { listLibrariesJSON, listLibrariesRemote = false, false })
	listLibrariesRemote = true

	cmd, buf := newListTestCmd()
	require.NoError(t, listLibraries(cmd), "one library's store error must not abort the whole listing")
	require.Contains(t, buf.String(), "working")
	require.Contains(t, buf.String(), "broken")
}

func TestListLibrariesEmptyWorkspace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: empty-ws\nlayout: flat\n"), 0o644))
	t.Chdir(dir)

	cmd, buf := newListTestCmd()
	require.NoError(t, listLibraries(cmd))
	require.Contains(t, buf.String(), "No libraries")
}
