package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// excludingWorkspace lays down two modules and an environment that withdraws
// what exclusions says: shop (api, vault, cache) and auth (hop, local).
func excludingWorkspace(t *testing.T, exclusions string) *resources.Workspace {
	t.Helper()
	root := t.TempDir()
	service := func(name, extra string) string {
		return "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.ai\n" + extra
	}
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: acme
layout: modules
modules:
  - name: shop
  - name: auth
environments:
  - name: local
` + exclusions,
		"modules/shop/module.codefly.yaml":                 "kind: module\nname: shop\nservice-entry: api\nservices:\n    - name: api\n    - name: vault\n    - name: cache\n",
		"modules/shop/services/api/service.codefly.yaml":   service("api", "service-dependencies:\n  - name: vault\n"),
		"modules/shop/services/vault/service.codefly.yaml": service("vault", ""),
		"modules/shop/services/cache/service.codefly.yaml": service("cache", ""),
		"modules/auth/module.codefly.yaml":                 "kind: module\nname: auth\nservices:\n    - name: hop\n    - name: local\n",
		"modules/auth/services/hop/service.codefly.yaml":   service("hop", ""),
		"modules/auth/services/local/service.codefly.yaml": service("local", ""),
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	require.NoError(t, err)
	return workspace
}

func useEnvironmentName(t *testing.T, value string) {
	t.Helper()
	previous := environmentName
	t.Cleanup(func() { environmentName = previous })
	environmentName = value
}

// A service the environment withdraws is no root, and a module withdrawn whole
// contributes none: the graph is built without them, so a root naming one would
// be a sort target the graph does not hold ("cannot topologically sort to").
func TestCompositionRootsLeaveOutWhatTheEnvironmentWithdraws(t *testing.T) {
	useEnvironmentName(t, "local")
	workspace := excludingWorkspace(t, `    exclude-services:
      - service: shop/vault
        withdrawn-from: [shop/api]
      - service: auth/hop
      - service: auth/local
`)
	roots, err := CompositionRoots(context.Background(), workspace)
	require.NoError(t, err)
	require.Equal(t, []string{"shop/api", "shop/cache"}, roots)

	// Without exclusions every service of every module is a root.
	useEnvironmentName(t, "local")
	all := excludingWorkspace(t, "")
	roots, err = CompositionRoots(context.Background(), all)
	require.NoError(t, err)
	require.Equal(t, []string{"shop/api", "shop/vault", "shop/cache", "auth/hop", "auth/local"}, roots)
}
