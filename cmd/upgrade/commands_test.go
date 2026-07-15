package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

func TestUpgradeCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{ServiceCmd, WorkspaceCmd, SecurityCmd} {
		if command.RunE == nil || command.Run != nil {
			t.Errorf("%s is not using RunE exclusively", command.Name())
		}
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"one", "two"}); err == nil {
		t.Fatal("service accepted multiple names")
	}
	if err := WorkspaceCmd.Args(WorkspaceCmd, []string{"unexpected"}); err == nil {
		t.Fatal("workspace accepted an argument")
	}
	if err := SecurityCmd.Args(SecurityCmd, []string{"unexpected"}); err == nil {
		t.Fatal("security accepted an argument")
	}
}

func TestGoModulesPropagatesWalkErrorsAndSkipsFixtures(t *testing.T) {
	if _, err := goModules(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing root returned no walk error")
	}

	root := t.TempDir()
	for _, file := range []string{
		filepath.Join(root, "go.mod"),
		filepath.Join(root, "nested", "go.mod"),
		filepath.Join(root, "testdata", "fixture", "go.mod"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("module example.com/test\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	modules, err := goModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 2 {
		t.Fatalf("modules = %v, want root and nested only", modules)
	}
}

func TestRunGoPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runGo(ctx, t.TempDir(), "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runGo error = %v, want context cancellation", err)
	}
}

func TestUpgradeCommandsReturnMissingWorkspaceErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := ServiceCmd.RunE(ServiceCmd, []string{"service"}); err == nil {
		t.Fatal("service upgrade without workspace returned success")
	}
	if err := WorkspaceCmd.RunE(WorkspaceCmd, nil); err == nil {
		t.Fatal("workspace upgrade without workspace returned success")
	}
}

func TestUpgradeServiceRejectsNilBoundaries(t *testing.T) {
	ctx := context.Background()
	workspace := &resources.Workspace{Name: "workspace"}
	module := &resources.Module{Kind: resources.ModuleKind, Name: "module"}
	service := &resources.Service{Name: "service"}

	for name, test := range map[string]func() error{
		"workspace": func() error {
			_, err := upgradeService(ctx, nil, module, service)
			return err
		},
		"module": func() error {
			_, err := upgradeService(ctx, workspace, nil, service)
			return err
		},
		"service": func() error {
			_, err := upgradeService(ctx, workspace, module, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := test(); err == nil {
				t.Fatal("nil boundary returned success")
			}
		})
	}
}
