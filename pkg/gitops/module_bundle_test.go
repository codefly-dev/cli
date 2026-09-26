package gitops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

func TestTransportNeutralModuleWorkspaceRemovesGitOpsAuthority(t *testing.T) {
	workspace := &resources.Workspace{
		Name: "workspace",
		Environments: []*resources.Environment{resourceEnvironment(t, &environments.Environment{
			Name:      "production",
			Namespace: "payments",
			Cluster: &environments.EnvironmentCluster{
				Kind:       "eks",
				Kubeconfig: "/host/kubeconfig",
				Context:    "production-admin",
			},
			Registry: &environments.EnvironmentRegistry{
				URL:  "621829027644.dkr.ecr.eu-west-1.amazonaws.com/payments",
				Auth: "ecr",
			},
			Gitops: &environments.EnvironmentGitops{
				RepoURL:      "https://github.com/codefly-dev/manifests.git",
				FetchRepoURL: "ssh://git@github.com/codefly-dev/manifests.git",
				Path:         "environments",
				Branch:       "main",
			},
			Secrets: []*resources.EnvironmentSecretProvider{{
				Kind:    "1password",
				Account: "private-account",
			}},
		})},
	}
	setWorkspaceGitops(t, workspace, &environments.EnvironmentGitops{
		RepoURL: "https://github.com/codefly-dev/manifests.git",
		Path:    "environments", Branch: "main",
	})
	sanitized, err := encodeTransportNeutralModuleWorkspace(workspace, "payments")
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

// A module bundle generator renders one module and looks a service up by the
// name that module knows it under, so a module-qualified key is re-keyed bare for
// the module it names and dropped for every other: another module's same-named
// entry would otherwise replace the wrong service's address and secrets.
func TestTransportNeutralModuleWorkspaceScopesManagedServicesToTheModule(t *testing.T) {
	workspace := &resources.Workspace{
		Name: "workspace",
		Environments: []*resources.Environment{resourceEnvironment(t, &environments.Environment{
			Name:      "production",
			Namespace: "payments",
			ManagedServices: map[string]environments.EnvironmentManagedService{
				"payments/redis": {Kind: "redis", ExternalName: "payments.cache.example", Port: 6379},
				"catalog/redis":  {Kind: "redis", ExternalName: "catalog.cache.example", Port: 6379},
				"warehouse":      {Kind: "postgres", ExternalName: "warehouse.example", Port: 5432},
			},
		})},
	}

	encoded, err := encodeTransportNeutralModuleWorkspace(workspace, "payments")
	if err != nil {
		t.Fatal(err)
	}
	managed := decodedModuleManagedServices(t, encoded)

	redis, ok := managed["redis"].(map[string]any)
	if !ok {
		t.Fatalf("payments/redis was not re-keyed bare: %#v", managed)
	}
	if redis["external-name"] != "payments.cache.example" {
		t.Errorf("redis external-name = %v, want this module's entry", redis["external-name"])
	}
	if _, leaked := managed["payments/redis"]; leaked {
		t.Errorf("a qualified key reached the generator: %#v", managed)
	}
	if _, leaked := managed["catalog/redis"]; leaked {
		t.Errorf("another module's entry reached the generator: %#v", managed)
	}
	// A bare key is unambiguous within one module, so it is carried through.
	if _, ok := managed["warehouse"]; !ok {
		t.Errorf("a bare key was dropped: %#v", managed)
	}
}

func decodedModuleManagedServices(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	var decoded struct {
		Environments []struct {
			ManagedServices map[string]any `yaml:"managed-services"`
		} `yaml:"environments"`
	}
	if err := yaml.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Environments) != 1 {
		t.Fatalf("environments = %d, want 1", len(decoded.Environments))
	}
	return decoded.Environments[0].ManagedServices
}
