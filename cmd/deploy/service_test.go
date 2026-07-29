package deploy

import "testing"

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
		if command.RunE == nil || command.Run != nil {
			t.Fatalf("gitops %s is not exclusively RunE", command.Name())
		}
	}
	for _, name := range []string{"render", "plan", "publish", "observe", "rollback"} {
		if !names[name] {
			t.Errorf("gitops %s command is missing", name)
		}
	}
}
