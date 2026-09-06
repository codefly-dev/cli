package publish

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func writeWorkspaceWithLibrary(t *testing.T, libraryYAML string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte(`name: test-ws
layout: flat
libraries:
  publish:
    go: {owner: codefly-dev}
    typescript: {registry: "http://127.0.0.1:1", scope: "@codefly-dev"}
    python: {owner: codefly-dev}
`), 0o644))
	libDir := filepath.Join(dir, "libraries", "authkit")
	require.NoError(t, os.MkdirAll(libDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(libDir, "library.codefly.yaml"), []byte(libraryYAML), 0o644))
	for rel, content := range files {
		path := filepath.Join(libDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return dir
}

func newTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	return cmd, buf
}

func resetPublishLibraryFlags() {
	publishLibraryVersion = ""
	publishLibraryLanguages = nil
	publishLibraryDryRun = false
}

func TestLibraryCommandReturnsErrorsThroughCobra(t *testing.T) {
	if libraryCmd.RunE == nil || libraryCmd.Run != nil {
		t.Fatal("publish library command is not exclusively RunE")
	}
	if err := libraryCmd.Args(libraryCmd, nil); err == nil {
		t.Fatal("publish library accepted zero positional arguments")
	}
	if err := libraryCmd.Args(libraryCmd, []string{"a", "b"}); err == nil {
		t.Fatal("publish library accepted two positional arguments")
	}
}

// TestPublishLibraryVersionMismatchNamesTheFile covers the no-network
// pre-flight gate: a language's version file that disagrees with the library
// version must fail before any store is even constructed, naming the file to
// fix.
func TestPublishLibraryVersionMismatchNamesTheFile(t *testing.T) {
	t.Cleanup(resetPublishLibraryFlags)
	dir := writeWorkspaceWithLibrary(t, `kind: library
name: authkit
version: 1.0.0
languages:
  - name: typescript
    path: typescript/
    exports:
      - "@codefly-dev/authkit"
`, map[string]string{
		"typescript/package.json": `{"name":"@codefly-dev/authkit","version":"0.9.0"}`,
	})
	t.Chdir(dir)

	cmd, _ := newTestCmd()
	err := publishLibrary(cmd, "authkit")
	require.Error(t, err)
	require.Contains(t, err.Error(), "package.json")
	require.Contains(t, err.Error(), "bump")
	require.Contains(t, err.Error(), "1.0.0")
}

// TestPublishLibraryDryRunPrintsInstallHintAndTouchesNoNetwork points
// GITHUB_TOKEN at garbage and the npm registry at an unreachable port
// (127.0.0.1:1, refused instantly) so that if --dry-run ever performed a
// git or HTTP call, this test would fail fast instead of hanging or passing
// by accident.
func TestPublishLibraryDryRunPrintsInstallHintAndTouchesNoNetwork(t *testing.T) {
	t.Cleanup(resetPublishLibraryFlags)
	dir := writeWorkspaceWithLibrary(t, `kind: library
name: authkit
version: 1.0.0
languages:
  - name: go
    path: go/
    exports:
      - github.com/codefly-dev/authkit-go
  - name: typescript
    path: typescript/
    exports:
      - "@codefly-dev/authkit"
`, map[string]string{
		"go/.keep":                "",
		"typescript/package.json": `{"name":"@codefly-dev/authkit","version":"1.0.0"}`,
	})
	t.Chdir(dir)
	t.Setenv("GITHUB_TOKEN", "not-a-real-token")

	publishLibraryDryRun = true
	cmd, buf := newTestCmd()
	err := publishLibrary(cmd, "authkit")
	require.NoError(t, err)
	require.Contains(t, buf.String(), "go get github.com/codefly-dev/authkit-go@v1.0.0")
	require.Contains(t, buf.String(), "npm install @codefly-dev/authkit@1.0.0")
}

func TestPublishLibraryRejectsExportIdentityMismatch(t *testing.T) {
	t.Cleanup(resetPublishLibraryFlags)
	dir := writeWorkspaceWithLibrary(t, `kind: library
name: authkit
version: 1.0.0
languages:
  - name: go
    path: go/
    exports:
      - github.com/wrong-owner/authkit-go
`, map[string]string{"go/.keep": ""})
	t.Chdir(dir)

	cmd, _ := newTestCmd()
	err := publishLibrary(cmd, "authkit")
	require.ErrorContains(t, err, "github.com/wrong-owner/authkit-go")
	require.ErrorContains(t, err, "github.com/codefly-dev/authkit-go")
}

func TestPublishLibraryMissingWorkspaceReturnsError(t *testing.T) {
	t.Cleanup(resetPublishLibraryFlags)
	t.Chdir(t.TempDir())
	cmd, _ := newTestCmd()
	require.Error(t, publishLibrary(cmd, "authkit"))
}

func TestPublishLibraryUnknownNameReturnsError(t *testing.T) {
	t.Cleanup(resetPublishLibraryFlags)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: test-ws\nlayout: flat\n"), 0o644))
	t.Chdir(dir)

	cmd, _ := newTestCmd()
	require.Error(t, publishLibrary(cmd, "does-not-exist"))
}
