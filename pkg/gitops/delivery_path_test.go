package gitops

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
)

// TestDeliveryModulePathIsTheGitopsPathOverModules is the contract of
// `gitops.path`: it names the directory, under the delivery repository's
// root, that holds `modules/`. No path means `deployments`, the layout a
// render leaves in the workspace, so `path: deployments` and no path are the
// same tree — and a path is never prefixed twice (obin-ai/platform-obin#128
// delivered `deployments/deployments/modules/tracing`).
func TestDeliveryModulePathIsTheGitopsPathOverModules(t *testing.T) {
	for gitopsPath, want := range map[string]string{
		"":                   "deployments/modules/tracing",
		"deployments":        "deployments/modules/tracing",
		"environments":       "environments/modules/tracing",
		"clusters/aws/stage": "clusters/aws/stage/modules/tracing",
	} {
		if got := deliveryModulePath(gitopsPath, "tracing"); got != want {
			t.Errorf("deliveryModulePath(%q) = %q, want %q", gitopsPath, got, want)
		}
	}
	if got := deliveryModulesPath(""); got != "deployments/modules" {
		t.Errorf("deliveryModulesPath(\"\") = %q", got)
	}
}

// TestGitopsPathOfIsOneSelectionForRenderAndPublish: the render records the
// owned path and the publish delivers to it through the same selection, so
// an environment overriding the path alone, inheriting the workspace's, or
// naming its own repository with no path lays the tree out the same way on
// both sides.
func TestGitopsPathOfIsOneSelectionForRenderAndPublish(t *testing.T) {
	defaults := &environments.EnvironmentGitops{RepoURL: "https://git.example.com/acme/manifests.git", Path: "environments"}
	cases := []struct {
		name     string
		env      *environments.Environment
		defaults *environments.EnvironmentGitops
		want     string
	}{
		{"no block inherits the workspace path", &environments.Environment{}, defaults, "environments"},
		{"a path alone overrides it", &environments.Environment{Gitops: &environments.EnvironmentGitops{Path: "staging"}}, defaults, "staging"},
		{"a repository of its own with no path is its root", &environments.Environment{Gitops: &environments.EnvironmentGitops{RepoURL: "https://git.example.com/acme/other.git"}}, defaults, ""},
		{"a repository of its own with a path", &environments.Environment{Gitops: &environments.EnvironmentGitops{RepoURL: "https://git.example.com/acme/other.git", Path: "deployments"}}, nil, "deployments"},
		{"nothing declared is the root", &environments.Environment{}, nil, ""},
	}
	for _, c := range cases {
		if got := gitopsPathOf(c.env, c.defaults); got != c.want {
			t.Errorf("%s: gitopsPathOf = %q, want %q", c.name, got, c.want)
		}
	}
}
