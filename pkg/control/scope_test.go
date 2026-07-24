package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceScopeRootsAtServiceDir(t *testing.T) {
	root := writeWorkspace(t)
	t.Chdir(root)
	ctx := context.Background()

	scope, err := New().Service(ctx, "backend/api")
	if err != nil {
		t.Fatal(err)
	}
	if scope.Name() != "backend/api" {
		t.Errorf("Name = %q, want backend/api", scope.Name())
	}
	if !strings.HasSuffix(filepath.Clean(scope.Dir()), filepath.Join("services", "api")) {
		t.Errorf("Dir = %q, want it to end in services/api", scope.Dir())
	}

	// A write through the scope lands under the SERVICE dir...
	if err := scope.WriteFile(ctx, "hello.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	data, err := scope.ReadFile(ctx, "hello.txt")
	if err != nil || string(data) != "hi" {
		t.Fatalf("scope read = %q, %v; want hi", data, err)
	}
	if _, err := os.Stat(filepath.Join(scope.Dir(), "hello.txt")); err != nil {
		t.Errorf("expected hello.txt under the service dir: %v", err)
	}
	// ...NOT under the workspace root.
	if _, err := os.Stat(filepath.Join(root, "hello.txt")); err == nil {
		t.Error("scope write leaked to the workspace root")
	}
}

func TestServiceScopeRejectsPathEscape(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	scope, err := New().Service(ctx, "backend/api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scope.ReadFile(ctx, "../../../../etc/passwd"); err == nil {
		t.Error("scope should reject a path escaping the service dir")
	}
}
