package gitops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

func TestModuleBundleRefusesRequiredDeployJobs(t *testing.T) {
	root := t.TempDir()
	bundle := moduleBundle{SchemaVersion: moduleBundleSchema, Module: "accounts", Environments: []moduleBundleEnvironment{{
		Name: "production", Namespace: "accounts", Cluster: "gke", Services: []string{"accounts"},
		DeployJobs: []json.RawMessage{json.RawMessage(`{"name":"role-catalog-import","service":"accounts","command":"role-catalog-import","after":["store"],"serviceEnvironment":["AUDIT_SINK"]}`)},
	}}}
	data, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "bundle.json"), data, 0o600))
	env := &environments.Environment{Name: "production", Namespace: "accounts", Cluster: &environments.EnvironmentCluster{Kind: "gke"}}
	_, _, err = loadSelectedModuleBundle(root, "accounts", env, "accounts", []InventoryUnit{{Name: "accounts"}})
	require.ErrorContains(t, err, "cannot execute module deploy jobs with dependency migration barriers; promotion refused")
	// A job declared for another environment does not block this one.
	bundle.Environments[0].Name = "staging"
	bundle.Environments = append(bundle.Environments, moduleBundleEnvironment{Name: env.Name, Namespace: env.Namespace, Cluster: "gke", Services: []string{"accounts"}})
	data, err = json.Marshal(bundle)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "bundle.json"), data, 0o600))
	_, _, err = loadSelectedModuleBundle(root, "accounts", env, "accounts", []InventoryUnit{{Name: "accounts"}})
	require.NoError(t, err)
}
