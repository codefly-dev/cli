package common

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestLoadWorkspaceReturnsErrorsInsteadOfExiting(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if _, err := LoadWorkspace(context.Background()); err == nil {
			t.Fatal("missing workspace returned nil error")
		}
	})

	t.Run("present", func(t *testing.T) {
		dir := t.TempDir()
		workspace := &resources.Workspace{Name: "test-workspace", Layout: resources.LayoutKindModules}
		if err := workspace.SaveToDirUnsafe(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		got, err := LoadWorkspace(context.Background())
		if err != nil {
			t.Fatalf("LoadWorkspace: %v", err)
		}
		if got.Name != workspace.Name {
			t.Fatalf("workspace name = %q, want %q", got.Name, workspace.Name)
		}
	})
}

func TestLoadRequiredEReturnsMissingWorkspaceError(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, _, err := LoadRequiredE(context.Background(), []string{"missing"}); err == nil {
		t.Fatal("missing workspace returned nil error")
	}
}

func TestLoadRequiredNonInteractiveENeverPromptsForAmbiguousService(t *testing.T) {
	root := filepath.Join("..", "..", "pkg", "orchestration", "testdata", "module-layout")
	t.Chdir(root)
	_, _, _, err := LoadRequiredNonInteractiveE(context.Background(), nil)
	if err == nil {
		t.Fatal("ambiguous headless service selection succeeded")
	}
	if !strings.Contains(err.Error(), "pass the service name explicitly") || !strings.Contains(err.Error(), "frontend") || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("headless ambiguity error = %q", err)
	}
}

func TestLoadActiveContextDoesNotReturnStaleWorkspace(t *testing.T) {
	ctx := context.Background()
	firstDir := t.TempDir()
	first := &resources.Workspace{Name: "first", Layout: resources.LayoutKindModules}
	if err := first.SaveToDirUnsafe(ctx, firstDir); err != nil {
		t.Fatal(err)
	}
	secondDir := t.TempDir()
	second := &resources.Workspace{Name: "second", Layout: resources.LayoutKindModules}
	if err := second.SaveToDirUnsafe(ctx, secondDir); err != nil {
		t.Fatal(err)
	}

	t.Chdir(firstDir)
	gotFirst, err := LoadActiveContext(ctx)
	if err != nil {
		t.Fatalf("load first active context: %v", err)
	}
	t.Chdir(secondDir)
	gotSecond, err := LoadActiveContext(ctx)
	if err != nil {
		t.Fatalf("load second active context: %v", err)
	}

	if gotFirst.Workspace.Name != "first" {
		t.Fatalf("first workspace = %q, want first", gotFirst.Workspace.Name)
	}
	if gotSecond.Workspace.Name != "second" {
		t.Fatalf("second workspace = %q, want second", gotSecond.Workspace.Name)
	}
}

func TestLoadModuleUsesCurrentEmptyModuleWithoutResolvingAService(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:   "test-workspace",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "coordination"},
		},
	}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}

	moduleDir := filepath.Join(root, "modules", "coordination")
	module := &resources.Module{
		Kind:              resources.ModuleKind,
		Name:              "coordination",
		ServiceReferences: []*resources.ServiceReference{},
	}
	module.WithDir(moduleDir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Chdir(moduleDir)

	got, err := LoadModule(ctx)
	if err != nil {
		t.Fatalf("LoadModule from empty module: %v", err)
	}
	if got.Name != "coordination" {
		t.Fatalf("module = %q, want coordination", got.Name)
	}

	gotWorkspace, gotRequired, err := LoadRequiredModuleE(ctx, nil)
	if err != nil {
		t.Fatalf("LoadRequiredModuleE from empty module: %v", err)
	}
	if gotWorkspace.Name != "test-workspace" {
		t.Fatalf("workspace = %q, want test-workspace", gotWorkspace.Name)
	}
	if gotRequired.Name != "coordination" {
		t.Fatalf("required module = %q, want coordination", gotRequired.Name)
	}
}

func TestLoadRequiredModuleEReturnsMissingWorkspaceError(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, err := LoadRequiredModuleE(context.Background(), []string{"missing"}); err == nil {
		t.Fatal("missing workspace returned nil error")
	}
}
