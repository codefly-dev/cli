package deploy

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/spf13/cobra"
)

const remoteGroupName = "remote"

func TestServiceCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("deploy service command is not exclusively RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("deploy service accepted two service names")
	}
}

func TestModuleCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ModuleCmd.RunE == nil || ModuleCmd.Run != nil {
		t.Fatal("deploy module command is not exclusively RunE")
	}
	if err := ModuleCmd.Args(ModuleCmd, []string{"one", "two"}); err == nil {
		t.Fatal("deploy module accepted two module names")
	}
}

func TestGitOpsCommandExposesCompletePromotionLifecycle(t *testing.T) {
	names := map[string]bool{}
	for _, command := range GitOpsCmd.Commands() {
		names[command.Name()] = true
		// Parent groups (e.g. remote) dispatch to their own leaves and carry no
		// RunE of their own; only leaves must be exclusively RunE.
		if command.HasSubCommands() {
			continue
		}
		if command.RunE == nil || command.Run != nil {
			t.Fatalf("gitops %s is not exclusively RunE", command.Name())
		}
	}
	for _, name := range []string{"snapshot", "render", "plan", "publish", "observe", "rollback", remoteGroupName} {
		if !names[name] {
			t.Errorf("gitops %s command is missing", name)
		}
	}
}

func TestGitOpsRemoteExposesFetchRemoteLifecycle(t *testing.T) {
	var remote *cobra.Command
	for _, command := range GitOpsCmd.Commands() {
		if command.Name() == remoteGroupName {
			remote = command
			break
		}
	}
	if remote == nil {
		t.Fatal("gitops remote command is missing")
	}
	names := map[string]bool{}
	for _, command := range remote.Commands() {
		names[command.Name()] = true
		if command.RunE == nil || command.Run != nil {
			t.Fatalf("gitops remote %s is not exclusively RunE", command.Name())
		}
	}
	for _, name := range []string{"plan", "up", "status", "down"} {
		if !names[name] {
			t.Errorf("gitops remote %s command is missing", name)
		}
	}
}

// No codefly:solution executor is published, so a render that could not obtain
// one must say so and name the way out — the module render path a composed
// solution already goes through — rather than leave the operator trying other
// --agent values. A failure from an executor that did run is reported as it was.
func TestSolutionRenderErrorPointsAtTheModuleRender(t *testing.T) {
	unavailable := fmt.Errorf("%w: resolve solution agent codefly.dev/solution-generic:0.0.1: not published", gitops.ErrSolutionExecutorUnavailable)
	err := solutionRenderError(unavailable, "lastlogin", "production")
	if !errors.Is(err, gitops.ErrSolutionExecutorUnavailable) {
		t.Fatalf("the sentinel was lost: %v", err)
	}
	for _, want := range []string{"not published", "no codefly:solution executor is published", "codefly deploy gitops render lastlogin --env production"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}

	ran := errors.New("package solution lastlogin: source has no Dockerfile")
	if err := solutionRenderError(ran, "lastlogin", "production"); !errors.Is(err, ran) || strings.Contains(err.Error(), "gitops render") {
		t.Errorf("an executor's own failure was rewritten: %v", err)
	}
}
