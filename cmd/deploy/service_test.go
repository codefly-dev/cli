package deploy

import (
	"testing"

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

// Both renders push and resolve immutable digests, and collection runs a
// container scanner that fails a render whose agent cannot serve image scope,
// which no released agent does yet — so each keeps the opt-in off by default.
func TestGitOpsRenderCommandsKeepImageEvidenceOptIn(t *testing.T) {
	for _, command := range []*cobra.Command{gitOpsSnapshotCmd, gitOpsRenderCmd} {
		flag := command.Flags().Lookup("image-sbom")
		if flag == nil {
			t.Fatalf("gitops %s does not accept --image-sbom", command.Name())
		}
		if flag.DefValue != "false" {
			t.Fatalf("gitops %s --image-sbom defaults to %q, want off", command.Name(), flag.DefValue)
		}
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
