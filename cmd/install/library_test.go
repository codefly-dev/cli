package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func newInstallTestCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	return cmd, buf
}

func resetInstallLibraryFlags() {
	destination = ""
	installLanguage = ""
}

func TestLibraryCommandReturnsErrorsThroughCobra(t *testing.T) {
	if LibraryCmd.RunE == nil || LibraryCmd.Run != nil {
		t.Fatal("install library command is not exclusively RunE")
	}
	if err := LibraryCmd.Args(LibraryCmd, nil); err == nil {
		t.Fatal("install library accepted zero positional arguments")
	}
	if err := LibraryCmd.Args(LibraryCmd, []string{"a", "b"}); err == nil {
		t.Fatal("install library accepted two positional arguments")
	}
}

func TestInstallLibraryRequiresNameAtConstraintSpec(t *testing.T) {
	t.Cleanup(resetInstallLibraryFlags)
	installLanguage = "go"
	cmd, _ := newInstallTestCmd()
	err := installLibrary(cmd, "authkit")
	require.ErrorContains(t, err, "<name>@<constraint>")
}

func TestInstallLibraryRequiresLanguageFlag(t *testing.T) {
	t.Cleanup(resetInstallLibraryFlags)
	cmd, _ := newInstallTestCmd()
	err := installLibrary(cmd, "authkit@^1.0.0")
	require.ErrorContains(t, err, "--language")
}

func TestInstallLibraryMissingWorkspaceReturnsError(t *testing.T) {
	t.Cleanup(resetInstallLibraryFlags)
	installLanguage = "go"
	t.Chdir(t.TempDir())
	cmd, _ := newInstallTestCmd()
	require.Error(t, installLibrary(cmd, "authkit@^1.0.0"))
}

func TestInstallLibraryUnconfiguredLanguageReturnsErrorBeforeNetwork(t *testing.T) {
	t.Cleanup(resetInstallLibraryFlags)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: test-ws\nlayout: flat\n"), 0o644))
	t.Chdir(dir)

	installLanguage = "go"
	cmd, _ := newInstallTestCmd()
	err := installLibrary(cmd, "authkit@^1.0.0")
	require.ErrorContains(t, err, "go.owner")
}
