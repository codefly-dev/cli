package sync

import "testing"

func TestLibraryDependenciesCommandReturnsErrorsThroughCobra(t *testing.T) {
	if LibraryDependenciesCmd.RunE == nil {
		t.Fatal("library-dependencies command has no RunE handler")
	}
	if LibraryDependenciesCmd.Run != nil {
		t.Fatal("library-dependencies command still has a Run handler")
	}
	if err := LibraryDependenciesCmd.Args(LibraryDependenciesCmd, []string{"extra"}); err == nil {
		t.Fatal("unexpected positional argument was accepted")
	}
}

func TestServiceCommandReturnsErrorsThroughCobra(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("sync service command is not exclusively RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("sync service accepted two service names")
	}
}
