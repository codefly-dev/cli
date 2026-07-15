package build

import "testing"

func TestServiceCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("build service command is not exclusively RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("build service accepted two service names")
	}
}

func TestModuleCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ModuleCmd.RunE == nil || ModuleCmd.Run != nil {
		t.Fatal("build module command is not exclusively RunE")
	}
	if err := ModuleCmd.Args(ModuleCmd, []string{"one", "two"}); err == nil {
		t.Fatal("build module accepted two module names")
	}
}
