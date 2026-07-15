package add

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestServiceAndApplicationCommandsValidateArguments(t *testing.T) {
	for name, command := range map[string]struct {
		valid   []string
		invalid [][]string
		args    func([]string) error
	}{
		"service": {
			valid:   []string{"api"},
			invalid: [][]string{nil, {"api", "extra"}},
			args:    func(args []string) error { return ServiceCmd.Args(ServiceCmd, args) },
		},
		"application": {
			valid:   []string{"web"},
			invalid: [][]string{nil, {"web", "extra"}},
			args:    func(args []string) error { return ApplicationCmd.Args(ApplicationCmd, args) },
		},
		"service dependency": {
			valid:   nil,
			invalid: [][]string{{"one", "two"}},
			args:    func(args []string) error { return ServiceDependencyCmd.Args(ServiceDependencyCmd, args) },
		},
		"application dependency": {
			valid:   nil,
			invalid: [][]string{{"unexpected"}},
			args:    func(args []string) error { return ApplicationDependencyCmd.Args(ApplicationDependencyCmd, args) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := command.args(command.valid); err != nil {
				t.Fatalf("valid arguments rejected: %v", err)
			}
			for _, args := range command.invalid {
				if err := command.args(args); err == nil {
					t.Fatalf("invalid arguments %q accepted", args)
				}
			}
		})
	}
}

func TestAddExistingServiceAndApplicationReturnErrors(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:   "test-workspace",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "billing"},
		},
	}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Join(root, "modules", "billing")
	module := &resources.Module{
		Kind:                  resources.ModuleKind,
		Name:                  "billing",
		ServiceReferences:     []*resources.ServiceReference{{Name: "api"}},
		ApplicationReferences: []*resources.ApplicationReference{{Name: "web"}},
	}
	module.WithDir(moduleDir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	previousOverride, previousAppOverride := override, appOverride
	override, appOverride = false, false
	t.Cleanup(func() {
		override, appOverride = previousOverride, previousAppOverride
	})

	if err := addService(ctx, "billing/api", "unused"); err == nil {
		t.Fatal("adding an existing service returned success")
	}
	if err := addApplication(ctx, "billing/web", "unused"); err == nil {
		t.Fatal("adding an existing application returned success")
	}
}

func TestDependencyHelpersReturnMissingWorkspaceErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	ctx := context.Background()
	if err := addServiceDependency(ctx, []string{"api"}); err == nil {
		t.Fatal("service dependency without workspace returned success")
	}
	if err := addApplicationDependency(ctx); err == nil {
		t.Fatal("application dependency without workspace returned success")
	}
}
