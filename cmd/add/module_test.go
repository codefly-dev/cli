package add

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

func TestModuleCommandRequiresExactlyOneName(t *testing.T) {
	for _, args := range [][]string{nil, {"one", "two"}} {
		if err := ModuleCmd.Args(ModuleCmd, args); err == nil {
			t.Fatalf("Args(%q) unexpectedly succeeded", args)
		}
	}
	if err := ModuleCmd.Args(ModuleCmd, []string{"billing"}); err != nil {
		t.Fatalf("valid module name rejected: %v", err)
	}
}

func TestAddExistingModuleReturnsError(t *testing.T) {
	dir := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "test",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "billing"}},
	}
	if err := workspace.SaveToDirUnsafe(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := addModule("billing"); err == nil {
		t.Fatal("adding an existing module returned success")
	}
}

func TestResourceCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{
		ModuleCmd,
		ServiceCmd,
		ApplicationCmd,
		ServiceDependencyCmd,
		ApplicationDependencyCmd,
		JobCmd,
		LibraryCmd,
		LibraryDependencyCmd,
	} {
		if command.RunE == nil {
			t.Errorf("%s has no RunE handler", command.Name())
		}
		if command.Run != nil {
			t.Errorf("%s still has a Run handler", command.Name())
		}
	}
}
