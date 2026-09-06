package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/librarystore"
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

// TestInstallIntoTypeScriptConfiguresRegistryBeforeInvokingNpm proves the
// --destination install path for a TypeScript export wires the store's
// registry/scope (and, implicitly, its credential) into destination before
// running npm — a bare `npm install <pkg>@<version>` would otherwise resolve
// against whatever registry destination's ambient npm config points at
// (typically registry.npmjs.org), not the registry (GitHub Packages by
// default, which requires authentication even to read a public package) the
// package was actually published to. Exercises prepareNpmDestination
// directly — the exact step installInto's TypeScript branch runs before
// invoking npm — rather than shelling out to the real npm CLI against an
// unreachable registry, which would pass but take a minute-plus of npm's own
// network-error retries to do so.
func TestInstallIntoTypeScriptConfiguresRegistryBeforeInvokingNpm(t *testing.T) {
	dir := t.TempDir()
	cfg := librarystore.StoreConfig{NpmRegistry: "https://npm.pkg.github.com", NpmScope: "@codefly-dev"}

	env, err := prepareNpmDestination(dir, cfg)
	require.NoError(t, err)
	require.Len(t, env, 1)
	require.True(t, strings.HasPrefix(env[0], "NODE_AUTH_TOKEN="), "env %v must carry NODE_AUTH_TOKEN for npm to authenticate", env)

	data, err := os.ReadFile(filepath.Join(dir, ".npmrc"))
	require.NoError(t, err, "the TypeScript install path must configure destination's registry mapping before running npm")
	require.Contains(t, string(data), "@codefly-dev:registry=https://npm.pkg.github.com")
	require.Contains(t, string(data), "_authToken=${NODE_AUTH_TOKEN}")
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
