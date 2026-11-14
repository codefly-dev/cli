package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/spf13/cobra"
)

// Every command the CLI runs hands core's agent loader the developer's gh
// login as a source when the environment holds no token, so a composition
// whose agents come from private releases renders without one exported by
// hand. The fake gh answers "not logged in", so no token reaches the network
// and the source shows itself in what the loader reports it tried.
func TestEveryCommandHandsTheAgentLoaderTheGHLogin(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'no oauth token found for github.com\\n' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Chdir(t.TempDir())
	resetReleaseTokenSource(t)

	if _, tried := manager.ReleaseToken(); strings.Contains(strings.Join(tried, ", "), "gh CLI login") {
		t.Fatalf("before the CLI's pre-run the loader already tries %v; want the environment alone", tried)
	}
	command := &cobra.Command{Use: "probe"}
	command.SetContext(context.Background())
	if err := RootCmd.PersistentPreRunE(command, nil); err != nil {
		t.Fatal(err)
	}
	if token, tried := manager.ReleaseToken(); token != "" || !strings.Contains(strings.Join(tried, ", "), "gh CLI login for github.com (gh is not logged in to it") {
		t.Fatalf("after the CLI's pre-run the loader reads %q and tried %v; want the gh login asked", token, tried)
	}
	t.Setenv("GH_TOKEN", "from-environment")
	if token, _ := manager.ReleaseToken(); token != "from-environment" {
		t.Fatalf("the loader reads %q with GH_TOKEN set; want the environment first", token)
	}
}

// resetReleaseTokenSource clears the source a command's pre-run installs in
// this process, now and when the test ends, so no later test's agent download
// asks the developer's real gh.
func resetReleaseTokenSource(t *testing.T) {
	t.Helper()
	manager.UseReleaseTokenSource(nil)
	t.Cleanup(func() { manager.UseReleaseTokenSource(nil) })
}
