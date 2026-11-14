package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/spf13/cobra"
)

// Every command the CLI runs hands core's agent loader the developer's gh
// login when the environment holds no token, so a composition whose agents
// come from private releases renders without one exported by hand.
func TestEveryCommandHandsTheAgentLoaderTheGHLogin(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$*\" = \"auth token --hostname github.com\" ] || exit 1\nprintf 'gho_fromlogin\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Chdir(t.TempDir())
	// Another test's command may have run the pre-run in this process.
	manager.UseReleaseTokenSource(nil)
	t.Cleanup(func() { manager.UseReleaseTokenSource(nil) })

	if token, tried := manager.ReleaseToken(); token != "" || len(tried) == 0 {
		t.Fatalf("before the CLI's pre-run the loader reads %q (tried %v); want the environment alone", token, tried)
	}
	command := &cobra.Command{Use: "probe"}
	command.SetContext(context.Background())
	if err := RootCmd.PersistentPreRunE(command, nil); err != nil {
		t.Fatal(err)
	}
	if token, _ := manager.ReleaseToken(); token != "gho_fromlogin" {
		t.Fatalf("the loader reads %q after the CLI's pre-run; want the gh login", token)
	}
	t.Setenv("GH_TOKEN", "from-environment")
	if token, _ := manager.ReleaseToken(); token != "from-environment" {
		t.Fatalf("the loader reads %q with GH_TOKEN set; want the environment first", token)
	}
}
