package orchestration

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A render or a deploy refuses to start while an invocation-scoped override is
// set.
//
// The carrier is core's private SDK-to-CLI channel, for an integration harness
// running against a throwaway workspace. Core applies it unconditionally, and
// this PR makes an overridden group composition-root — the run itself is
// supplying the value — so it reaches every service of the composition. A stray
// carrier in a CI job therefore writes its values into every committed manifest.
// `docs/orchestration.md` said not to set it there, which is advice, not a
// guard. (Layer-5 round-five NEW-3.)
func TestARenderRefusesToRunWithAnInvocationOverrideSet(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	payments, err := workspace.LoadModuleFromName(ctx, "payments")
	require.NoError(t, err)
	worker, err := payments.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)

	encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{
		{Name: "work-context", Key: "authority-endpoint", Value: "https://injected.example"},
	})
	require.NoError(t, err)
	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)

	for _, mode := range []Mode{SnapshotMode, DeployMode} {
		t.Run("refused in "+string(mode), func(t *testing.T) {
			_, err := NewFlow(ctx, workspace, payments, worker, env, mode)
			require.Error(t, err, "an override must not reach a committed manifest or a deployed workload")
			require.Contains(t, err.Error(), resources.WorkspaceConfigurationOverridesEnvironment)
		})
	}

	// A local run is exactly what the carrier is for, so it is untouched.
	t.Run("allowed in a run", func(t *testing.T) {
		flow, err := NewFlow(ctx, workspace, payments, worker, env, RunMode)
		require.NoError(t, err, "the carrier is a local-run mechanism and must keep working")
		t.Cleanup(func() { _ = flow.Stop() })
	})
}

// The standalone plan gate fails closed on an invocation it cannot read.
//
// `CODEFLY__WORKSPACE_CONFIGURATION_OVERRIDES=not-json` is an invalid
// invocation. The gate used to log the reader failure at Debug and check the
// configurations on disk instead — so it validated something the run would never
// resolve and reported a pass. Layer 4 found it; this is its reproduction,
// committed, because the code was fixed and nothing pinned it. (Layer-5
// round-five NEW-4.)
func TestAnInvalidInvocationCannotPassTheStandalonePlanGate(t *testing.T) {
	ctx := context.Background()
	workspace := referenceValidityWorkspace(t, "platform/authority/admin", "public")
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	consumer, err := loadService(ctx, t, workspace, "payments", "worker")
	require.NoError(t, err)

	// The same plan passes while the invocation is readable, so the refusal is
	// about the invocation and not about the composition.
	require.NoError(t, PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{consumer}, true))

	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, "not-json")
	err = PlanConfigurationReferences(ctx, workspace, env, []*resources.Service{consumer}, true)
	require.Error(t, err, "an invocation whose overrides cannot be read must not pass the plan")
	require.Contains(t, err.Error(), "cannot load the workspace configurations this invocation resolves")
}

// The flow's gate fails closed the same way, on the other shape of unreadable:
// a value the group declares as supplied per profile that the selected profile
// does not supply.
//
// The fallback that used to stand there carried NO composition-root names, so a
// load failure anywhere in the configurations turned the gate into a
// declared-groups-only check without saying so. Both fail-closed paths are
// pinned now — this one and the standalone plan above — because the mutation
// that swallows either error survived every committed test.
func TestTheFlowPlanGateFailsClosedOnAnUnreadableInvocation(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: payments\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		// Declared as supplied per profile, and this profile supplies nothing.
		"configurations/local/pending.env": "MODE=${profile}\n",
	})
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	payments, err := workspace.LoadModuleFromName(ctx, "payments")
	require.NoError(t, err)
	worker, err := payments.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, payments, worker, env, RunMode)
	require.NoError(t, err, "NewFlow must not refuse: a build or a sync resolves no configuration")
	t.Cleanup(func() { _ = flow.Stop() })

	err = flow.InitManagers(ctx)
	require.Error(t, err, "a run whose configurations cannot be read must not pass the gate")
	require.Contains(t, err.Error(), "cannot load the workspace configurations this invocation resolves")
	require.Empty(t, flow.hub.managers, "no service of the run set was created")
	// And it is not reported as an unresolved reference: nothing was checked.
	var unresolved *configurations.UnresolvedReferencesError
	require.False(t, errors.As(err, &unresolved), "the gate must say it could not check, not that a reference failed")
}
