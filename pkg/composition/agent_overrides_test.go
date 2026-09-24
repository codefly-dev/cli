package composition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func agentOverrideService(name, agent, version string) string {
	return "kind: service\nname: " + name + "\nversion: 0.0.0\nagent:\n  kind: runtime::service\n  name: " + agent + "\n  version: " + version + "\n  publisher: codefly.dev\n"
}

// writeAgentOverrideWorkspace composes two local modules whose go-grpc services
// pin 0.1.46 and 0.1.45, plus a nextjs service, under the given overrides block.
func writeAgentOverrideWorkspace(t *testing.T, overrides string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"workspace.codefly.yaml":                               "name: deploy\nlayout: modules\nmodules:\n  - name: saas\n  - name: billing\n" + overrides,
		"modules/saas/module.codefly.yaml":                     "kind: module\nname: saas\nservices:\n  - name: accounts\n  - name: frontend\n",
		"modules/saas/services/accounts/service.codefly.yaml":  agentOverrideService("accounts", "go-grpc", "0.1.46"),
		"modules/saas/services/frontend/service.codefly.yaml":  agentOverrideService("frontend", "nextjs", "0.0.159"),
		"modules/billing/module.codefly.yaml":                  "kind: module\nname: billing\nservices:\n  - name: ledger\n",
		"modules/billing/services/ledger/service.codefly.yaml": agentOverrideService("ledger", "go-grpc", "0.1.45"),
	}
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	return dir
}

func loadAgentOverrideWorkspace(t *testing.T, overrides string) *resources.Workspace {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), writeAgentOverrideWorkspace(t, overrides))
	require.NoError(t, err)
	return workspace
}

// The owner's case: a module pins go-grpc 0.1.46 and the workspace moves it to
// 0.1.47 without re-tagging the module.
func TestAgentOverrideResolvesTheOverriddenVersion(t *testing.T) {
	ctx := context.Background()
	workspace := loadAgentOverrideWorkspace(t, "agent-overrides:\n  codefly.dev/go-grpc: 0.1.47\n")

	mod, err := workspace.LoadModuleFromName(ctx, "saas")
	require.NoError(t, err)
	accounts, err := mod.LoadServiceFromName(ctx, "accounts")
	require.NoError(t, err)
	require.Equal(t, "0.1.47", accounts.Agent.Version)
	require.Equal(t, "go-grpc", accounts.Agent.Name)
	require.Equal(t, "codefly.dev", accounts.Agent.Publisher)
	frontend, err := mod.LoadServiceFromName(ctx, "frontend")
	require.NoError(t, err)
	require.Equal(t, "0.0.159", frontend.Agent.Version, "another agent is not moved")

	uses, err := ResolveAgentOverrides(ctx, workspace)
	require.NoError(t, err)
	require.Len(t, uses, 1)
	require.Equal(t, []string{"billing/ledger", "saas/accounts"}, uses[0].Services)
	require.Equal(t, []string{"0.1.45", "0.1.46"}, uses[0].ModulePins)
	require.Equal(t, "agent codefly.dev/go-grpc overridden to 0.1.47 by workspace.codefly.yaml (2 services; module pins: 0.1.45, 0.1.46)", uses[0].Line())
}

func TestAgentOverrideRefusesAKeyNoComposedServiceUses(t *testing.T) {
	workspace := loadAgentOverrideWorkspace(t, "agent-overrides:\n  codefly.dev/go-grpc: 0.1.47\n  codefly.dev/go-grcp: 0.1.47\n")
	_, err := ResolveAgentOverrides(context.Background(), workspace)
	require.Error(t, err)
	require.Contains(t, err.Error(), "codefly.dev/go-grcp")
	require.NotContains(t, err.Error(), "codefly.dev/go-grpc,")
}

func TestAgentOverrideRefusesMalformedEntries(t *testing.T) {
	for _, block := range []string{
		"agent-overrides:\n  go-grpc: 0.1.47\n",
		"agent-overrides:\n  codefly.dev/go-grpc: latest\n",
	} {
		workspace := loadAgentOverrideWorkspace(t, block)
		_, err := ResolveAgentOverrides(context.Background(), workspace)
		require.Error(t, err, block)
		require.True(t, strings.Contains(err.Error(), resources.AgentOverridesKey), err.Error())
	}
}

func TestNoAgentOverridesIsNothing(t *testing.T) {
	uses, err := ResolveAgentOverrides(context.Background(), loadAgentOverrideWorkspace(t, ""))
	require.NoError(t, err)
	require.Empty(t, uses)
}
