package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `codefly agent deps --link` edits the monorepo root's go.work. Its `go work`
// commands inherited the caller's environment, so with GOWORK=off exported —
// what CI and every hermetic shell sets — `go work use` answered "no go.work
// file found" for the file linkLocal had just created. The commands name the
// workspace they edit, whatever the caller's GOWORK says.
func TestLinkAndUnlinkEditTheRootWorkspaceWhateverGOWORKSays(t *testing.T) {
	for _, gowork := range []string{"off", ""} {
		t.Run("GOWORK="+gowork, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			agent := filepath.Join(root, "agents", "services", "myagent")
			for dir, module := range map[string]string{
				filepath.Join(root, "core"): "github.com/codefly-dev/core",
				agent:                       "example.com/myagent",
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+module+"\n\ngo 1.25\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GOWORK", gowork)
			t.Setenv("GOFLAGS", "-mod=mod")
			ctx := context.Background()

			if err := linkLocal(ctx, agent); err != nil {
				t.Fatalf("link: %v", err)
			}
			work, err := os.ReadFile(filepath.Join(root, "go.work"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(work), "./core") || !strings.Contains(string(work), "agents/services/myagent") {
				t.Fatalf("go.work does not use core and the agent:\n%s", work)
			}

			if err := unlinkLocal(ctx, agent); err != nil {
				t.Fatalf("unlink: %v", err)
			}
			work, err = os.ReadFile(filepath.Join(root, "go.work"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(work), "./core") || strings.Contains(string(work), "myagent") {
				t.Fatalf("unlink must drop the agent and keep core:\n%s", work)
			}
		})
	}
}
