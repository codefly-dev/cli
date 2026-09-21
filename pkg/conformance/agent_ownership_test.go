package conformance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRequiredCLIGatesDoNotInstallReleasedAgents(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	data := readFile(t, filepath.Join(repositoryRoot(t), ".github/workflows/go.yml"))
	if err := yaml.Unmarshal([]byte(data), &workflow); err != nil {
		t.Fatal(err)
	}
	for _, step := range workflow.Jobs["quality"].Steps {
		if strings.Contains(step.Run, "codefly agent install") || strings.Contains(step.Run, "codefly.dev/") {
			t.Fatalf("CLI required gate depends on a released agent: %s", step.Run)
		}
		for key, value := range step.Env {
			if key == "CODEFLY_SOURCE_QUALIFY_AGENT" || strings.Contains(value, "codefly.dev/") {
				t.Fatalf("CLI gate selects a released agent through %s", key)
			}
		}
	}
	row, ok := Default().Row("linux-amd64-native-control")
	if !ok || len(row.Agents) != 0 || row.Backend != "native" {
		t.Fatal("control gate must describe host protocol behavior, not a released provider")
	}
}

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
