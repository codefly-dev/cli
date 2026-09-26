package ci

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A gate that runs a phase resolving workspace configurations refuses a
// configuration error of a planned service before any phase runs; a gate whose
// phases resolve none is not refused.
func TestCIGateRefusesUnresolvedConfigurationReferencesBeforeAnyPhase(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"workspace.codefly.yaml":              "name: demo\nlayout: modules\nmodules:\n    - name: backend\n",
		"modules/backend/module.codefly.yaml": "kind: module\nname: backend\nservices:\n    - name: api\n",
		"modules/backend/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: backend\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - platform\n",
		"configurations/local/platform.env": "store-endpoint=${endpoint:store/db/tcp}\n",
	} {
		path := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	plan := &Plan{Services: []PlannedService{{Service: "backend/api"}}}

	err = validateConfigurationReferences(ctx, workspace, plan, []string{ciPhaseLint, ciPhaseTest, ciPhaseBuild})
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
	require.Len(t, unresolved.References, 1)
	require.Equal(t, "backend/api", unresolved.References[0].Consumer)
	require.Equal(t, "store-endpoint", unresolved.References[0].Key)
	require.Equal(t, "store/db", unresolved.References[0].Producer)

	// Lint and compile drive a runtime flow that reaches RuntimeInit, so both
	// resolve the workspace configurations and are refused by the same error —
	// a gate of either alone is checked.
	for _, phases := range [][]string{{ciPhaseLint}, {ciPhaseCompile}, {ciPhaseLint, ciPhaseBuild}} {
		require.Error(t, validateConfigurationReferences(ctx, workspace, plan, phases), "phases %v resolve configurations", phases)
	}
	// Nothing in this set resolves a configuration.
	require.NoError(t, validateConfigurationReferences(ctx, workspace, plan,
		[]string{ciPhaseSyncDrift, ciPhaseAudit, ciPhaseSBOM, ciPhaseBuild}))
}

// --fail-fast=false asks every remaining phase to contribute its own evidence.
// A reference error refuses only the phases that resolve configurations, so the
// rest must survive the filter the gate applies to them.
func TestConfigurationReferenceRefusalKeepsThePhasesItDoesNotRefuse(t *testing.T) {
	all := []string{ciPhaseSyncDrift, ciPhaseLint, ciPhaseCompile, ciPhaseTest, ciPhaseAudit, ciPhaseSBOM, ciPhaseBuild}
	require.Equal(t,
		[]string{ciPhaseSyncDrift, ciPhaseAudit, ciPhaseSBOM, ciPhaseBuild},
		slices.DeleteFunc(slices.Clone(all), phaseResolvesConfigurations))
}
