package gitops

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

func resourceEnvironment(t *testing.T, env *environments.Environment) *resources.Environment {
	t.Helper()
	resource, err := env.Resource()
	if err != nil {
		t.Fatal(err)
	}
	return resource
}

func selectedEnvironment(t *testing.T, workspace *resources.Workspace, name string) *environments.Environment {
	t.Helper()
	env, err := environments.Select(workspace, name)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func setWorkspaceGitops(t *testing.T, workspace *resources.Workspace, gitops *environments.EnvironmentGitops) {
	t.Helper()
	if workspace.Extensions == nil {
		workspace.Extensions = make(map[string]resources.YAMLValue)
	}
	var node yaml.Node
	if err := node.Encode(gitops); err != nil {
		t.Fatal(err)
	}
	workspace.Extensions["gitops"] = resources.YAMLValue{Node: node}
}
