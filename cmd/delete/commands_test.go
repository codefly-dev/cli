package delete

import (
	"context"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestDeleteCommandsRequireExactlyOneName(t *testing.T) {
	for _, command := range []struct {
		name string
		args func([]string) error
	}{
		{name: "module", args: func(args []string) error { return ModuleCmd.Args(ModuleCmd, args) }},
		{name: "service", args: func(args []string) error { return ServiceCmd.Args(ServiceCmd, args) }},
	} {
		t.Run(command.name, func(t *testing.T) {
			if err := command.args(nil); err == nil {
				t.Fatal("missing name unexpectedly accepted")
			}
			if err := command.args([]string{"one", "two"}); err == nil {
				t.Fatal("extra name unexpectedly accepted")
			}
			if err := command.args([]string{"one"}); err != nil {
				t.Fatalf("valid name rejected: %v", err)
			}
		})
	}
}

func TestDeleteMissingModuleReturnsError(t *testing.T) {
	dir := t.TempDir()
	workspace := &resources.Workspace{Name: "test", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := deleteModule("missing"); err == nil {
		t.Fatal("deleting a missing module returned success")
	}
}
