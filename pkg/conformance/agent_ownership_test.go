package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestHostCommandsDoNotScavengePostgresIPC(t *testing.T) {
	for _, relative := range []string{"cmd/run/service.go", "cmd/clear.go"} {
		t.Run(relative, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repositoryRoot(t), relative), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				if name, ok := node.(*ast.Ident); ok && name.Name == "ReapOrphanedPostgresIPC" {
					t.Error("host commands must not own PostgreSQL IPC cleanup")
				}
				return true
			})
		})
	}
}
