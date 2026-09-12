package list

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeRunnable(t *testing.T, dir, name, version, facilities string) {
	t.Helper()
	runnableDir := filepath.Join(dir, "runnables", name)
	require.NoError(t, os.MkdirAll(runnableDir, 0o755))
	declaration := `kind: runnable
name: ` + name + `
version: ` + version + `
agent:
  kind: codefly:runnable
  name: python
  version: 0.0.1
  publisher: codefly.dev
contract:
  protocol: codefly.runnable/v1
  input:
    fields:
      - name: text
        type: string
  output:
    fields:
      - name: count
        type: integer
entrypoint:
  handler: handler.py
execution:
  facilities: [` + facilities + `]
  timeout: 2m
  cancellation: signal
  recovery: recompute
`
	require.NoError(t, os.WriteFile(filepath.Join(runnableDir, "runnable.codefly.yaml"), []byte(declaration), 0o644))
}

func writeWorkspaceWithTwoRunnables(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	workspace := "name: test-ws\nlayout: flat\nrunnables:\n  - name: word-count\n  - name: summarize\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(workspace), 0o644))
	writeRunnable(t, dir, "word-count", "0.1.0", "native")
	writeRunnable(t, dir, "summarize", "1.2.3", "native, kubernetes")
	return dir
}

func TestRunnablesCommandReturnsErrorsThroughCobra(t *testing.T) {
	if RunnablesCmd.RunE == nil || RunnablesCmd.Run != nil {
		t.Fatal("list runnables command is not exclusively RunE")
	}
	if err := RunnablesCmd.Args(RunnablesCmd, []string{"extra"}); err == nil {
		t.Fatal("list runnables accepted a positional argument")
	}
}

func TestListRunnablesTableHasARowPerRunnable(t *testing.T) {
	t.Chdir(writeWorkspaceWithTwoRunnables(t))
	t.Cleanup(func() { listRunnablesJSON, listRunnablesModule = false, "" })

	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 3, "header + two runnable rows, got:\n%s", buf.String())
	require.Contains(t, lines[0], "VERSION")
	require.Contains(t, buf.String(), "word-count")
	require.Contains(t, buf.String(), "summarize")
	require.Contains(t, buf.String(), "native,kubernetes")
}

func TestListRunnablesJSONCarriesIdentityAndFacilities(t *testing.T) {
	t.Chdir(writeWorkspaceWithTwoRunnables(t))
	t.Cleanup(func() { listRunnablesJSON, listRunnablesModule = false, "" })
	listRunnablesJSON = true

	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))

	var entries []runnableListEntry
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entries))
	require.Len(t, entries, 2)
	byName := map[string]runnableListEntry{}
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	require.Equal(t, "1.2.3", byName["summarize"].Version)
	require.Equal(t, "codefly.dev/python:0.0.1", byName["summarize"].Agent)
	require.Equal(t, []string{"native", "kubernetes"}, byName["summarize"].Facilities)
	require.Equal(t, "test-ws", byName["word-count"].Module)
}

func TestListRunnablesUnknownModuleReturnsError(t *testing.T) {
	t.Chdir(writeWorkspaceWithTwoRunnables(t))
	t.Cleanup(func() { listRunnablesJSON, listRunnablesModule = false, "" })
	listRunnablesModule = "absent"

	cmd, _ := newListTestCmd()
	require.Error(t, listRunnables(cmd))
}

// TestListRunnablesInvalidDeclarationFails keeps an unloadable runnable a
// failure of the listing rather than a silently shorter list: a caller about
// to build or install what it finds must not be told the broken one is absent.
func TestListRunnablesInvalidDeclarationFails(t *testing.T) {
	dir := writeWorkspaceWithTwoRunnables(t)
	broken := filepath.Join(dir, "runnables", "summarize", "runnable.codefly.yaml")
	content, err := os.ReadFile(broken)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(broken, append(content, []byte("optionnal: true\n")...), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { listRunnablesJSON, listRunnablesModule = false, "" })

	cmd, _ := newListTestCmd()
	err = listRunnables(cmd)
	require.ErrorContains(t, err, "optionnal")
}

func TestListRunnablesEmptyWorkspace(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: empty-ws\nlayout: flat\n"), 0o644))
	t.Chdir(dir)
	t.Cleanup(func() { listRunnablesJSON, listRunnablesModule = false, "" })

	cmd, buf := newListTestCmd()
	require.NoError(t, listRunnables(cmd))
	require.Contains(t, buf.String(), "No runnables")
}

func TestListRunnablesMissingWorkspaceReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Cleanup(func() { listRunnablesJSON, listRunnablesModule = false, "" })

	cmd, _ := newListTestCmd()
	require.Error(t, listRunnables(cmd))
}
