package environments_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

func loadGenerators(t *testing.T, generate string) ([]environments.EnvironmentSecretGenerator, error) {
	t.Helper()
	root := t.TempDir()
	workspace := `name: platform
layout: modules
environments:
  - name: prod
    namespace: platform
    service-secrets:
      secret-store:
        name: cell-secrets
        kind: ClusterSecretStore
      generate:
` + generate
	if err := os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDeploymentWorkspace(context.Background(), root)
	if err != nil {
		return nil, err
	}
	return loaded.Environments[0].ServiceSecrets.Generate, nil
}

func TestSecretGeneratorsLoadAndNameTheStoredKeys(t *testing.T) {
	generators, err := loadGenerators(t, `        - scope: workspace
          configuration: internal-auth
          keys: [CODEFLY_INTERNAL_TOKEN]
        - scope: service
          configuration: postgres
          keys: [POSTGRES_USER]
          format: identifier
          services: [task-runtime/store]
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := generators[0].StoredKeys(nil); !slices.Equal(got, []string{"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__INTERNAL_AUTH__CODEFLY_INTERNAL_TOKEN"}) {
		t.Errorf("workspace stored keys = %v", got)
	}
	// Named services win over the workspace's; dashes become underscores exactly
	// as core names the variable.
	if got := generators[1].StoredKeys([]string{"other/store"}); !slices.Equal(got, []string{"CODEFLY__SERVICE_SECRET_CONFIGURATION__TASK_RUNTIME__STORE__POSTGRES__POSTGRES_USER"}) {
		t.Errorf("service stored keys = %v", got)
	}
	unnamed := &environments.EnvironmentSecretGenerator{Scope: "service", Configuration: "postgres", Keys: []string{"POSTGRES_PASSWORD"}}
	if got := unnamed.StoredKeys([]string{"a/store", "b/store"}); len(got) != 2 {
		t.Errorf("a generator naming no service covers %v, want every service's", got)
	}
}

func TestSecretGeneratorsRefuseAMalformedDeclaration(t *testing.T) {
	for name, declaration := range map[string]string{
		"unknown field":      "        - scope: workspace\n          configuration: x\n          key: [A]\n",
		"no keys":            "        - scope: workspace\n          configuration: x\n",
		"bad scope":          "        - scope: module\n          configuration: x\n          keys: [A]\n",
		"workspace services": "        - scope: workspace\n          configuration: x\n          keys: [A]\n          services: [a/b]\n",
		"bad service":        "        - scope: service\n          configuration: x\n          keys: [A]\n          services: [store]\n",
		"bad format":         "        - scope: workspace\n          configuration: x\n          keys: [A]\n          format: uuid\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadGenerators(t, declaration); err == nil || !strings.Contains(err.Error(), "generate") {
				t.Errorf("loaded %q, want a refusal naming the generate block: %v", declaration, err)
			}
		})
	}
}
