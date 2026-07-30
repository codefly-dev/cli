package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

func TestTransportNeutralModuleWorkspaceRemovesGitOpsAuthority(t *testing.T) {
	workspace := &resources.Workspace{
		Name: "workspace",
		Gitops: &resources.WorkspaceGitops{
			RepoURL: "https://github.com/codefly-dev/manifests.git",
			Path:    "environments",
			Branch:  "main",
		},
		Environments: []*resources.Environment{{
			Name:      "production",
			Namespace: "payments",
			Cluster: &resources.EnvironmentCluster{
				Kind:       "eks",
				Kubeconfig: "/host/kubeconfig",
				Context:    "production-admin",
			},
			Registry: &resources.EnvironmentRegistry{
				URL:  "621829027644.dkr.ecr.eu-west-1.amazonaws.com/payments",
				Auth: "ecr",
			},
			Gitops: &resources.EnvironmentGitops{
				RepoURL:      "https://github.com/codefly-dev/manifests.git",
				FetchRepoURL: "ssh://git@github.com/codefly-dev/manifests.git",
				Path:         "environments",
				Branch:       "main",
			},
			Secrets: []*resources.EnvironmentSecretProvider{{
				Kind:    "1password",
				Account: "private-account",
			}},
		}},
	}
	sanitized, err := encodeTransportNeutralModuleWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"gitops:",
		"github.com",
		"branch:",
		"kubeconfig:",
		"production-admin",
		"registry:",
		"amazonaws.com",
		"secrets:",
		"private-account",
	} {
		if strings.Contains(string(sanitized), forbidden) {
			t.Fatalf("sanitized workspace exposes %q:\n%s", forbidden, sanitized)
		}
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(sanitized, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["name"] != "workspace" {
		t.Fatalf("sanitized workspace lost topology: %#v", decoded)
	}
	environments, ok := decoded["environments"].([]any)
	if !ok || len(environments) != 1 {
		t.Fatalf("sanitized workspace environments = %#v", decoded["environments"])
	}
	environment, ok := environments[0].(map[string]any)
	if !ok || environment["name"] != "production" || environment["namespace"] != "payments" {
		t.Fatalf("sanitized environment lost topology: %#v", environments[0])
	}
}

func TestTransportNeutralModuleEnvironmentIsIsolated(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("KUBECONFIG", "/host/kubeconfig")
	t.Setenv("PULUMI_ACCESS_TOKEN", "secret")
	t.Setenv("CODEFLY_HOME", "/host/codefly")

	stage := t.TempDir()
	environment, err := transportNeutralModuleEnvironment(stage)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	for _, forbidden := range []string{
		"GITHUB_TOKEN=",
		"AWS_SECRET_ACCESS_KEY=",
		"KUBECONFIG=",
		"PULUMI_ACCESS_TOKEN=",
		"CODEFLY_HOME=",
	} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("module environment exposes %s", forbidden)
		}
	}
	for _, key := range []string{"HOME", "USERPROFILE", "TMPDIR", "TMP", "TEMP"} {
		expectedPrefix := key + "=" + stage + string(filepath.Separator)
		if !strings.Contains(joined, expectedPrefix) {
			t.Fatalf("module environment %s is not isolated:\n%s", key, joined)
		}
	}
	for _, directory := range []string{
		filepath.Join(stage, ".codefly-module-home"),
		filepath.Join(stage, ".codefly-module-tmp"),
	} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", directory)
		}
	}
}
