package orchestration

import (
	"context"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

// The composition root's runnable-bindings group reaches only the services that
// declare it (codefly-dev/cli#1256). `codefly generate runnable-bindings` writes
// one prepared value per operation of every composed module to the root, and
// the root's groups are otherwise run-wide: every service of a 100-operation
// composition was handed all of them and outgrew its process environment. The
// other root group still reaches all three services, so this narrows one group
// and leaves the root's inheritance as it was, on the run's and the render's
// shared resolution alike.
func TestRunnableBindingsReachOnlyTheServicesThatDeclareThem(t *testing.T) {
	agent := "agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n"
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: bindings\nlayout: modules\nmodules:\n    - name: payments\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\n" +
			"services:\n    - name: worker\n    - name: web\n    - name: api\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" + agent +
			"workspace-configuration-dependencies:\n    - " + runnablespkg.RunnableBindingsGroup + "\n",
		"modules/payments/services/web/service.codefly.yaml": "kind: service\nname: web\nversion: 0.0.0\nmodule: payments\n" + agent,
		"modules/payments/services/api/service.codefly.yaml":  "kind: service\nname: api\nversion: 0.0.0\nmodule: payments\n" + agent,
		"configurations/local/platform.env":                    "REGION=local\n",
		"configurations/local/runnable-bindings.env":           "PAYMENTS__WORKER_CONNECT_SETTLE={}\nPAYMENTS__WORKER_CONNECT_REFUND={}\n",
	})
	world, _ := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = RunMode })
	ctx := context.Background()
	received := func(name string) (ran, built []string, values int) {
		t.Helper()
		service, err := loadService(ctx, t, workspace, "payments", name)
		require.NoError(t, err)
		instance := &coreservices.Instance{Service: service}
		runConfs, err := (&Runner{world: world, instance: instance}).workspaceConfigurations(ctx, nil, &basev0.RuntimeContext{Kind: resources.RuntimeContextNative})
		require.NoError(t, err)
		buildConfs, err := (&Builder{world: world, instance: instance}).workspaceConfigurations(ctx, nil)
		require.NoError(t, err)
		for _, info := range workspaceConfigurationInfos(runConfs) {
			if info.GetName() == runnablespkg.RunnableBindingsGroup {
				values += len(info.GetConfigurationValues())
			}
		}
		return workspaceConfigurationNames(runConfs), workspaceConfigurationNames(buildConfs), values
	}

	ran, built, values := received("worker")
	require.ElementsMatch(t, []string{"platform", runnablespkg.RunnableBindingsGroup}, ran)
	require.ElementsMatch(t, ran, built, "the render hands the declaring service what the run does")
	require.Equal(t, 2, values, "the declaring service receives every prepared binding")
	for _, name := range []string{"web", "api"} {
		ran, built, _ = received(name)
		require.Equal(t, []string{"platform"}, ran, "%s declares no runnable-bindings and receives none", name)
		require.Equal(t, ran, built, "%s: the render hands it what the run does", name)
	}
	// The plan gate reads the root's names through the same rule, so it does not
	// hold a service to a group it never receives.
	_, rootGroups, err := WorkspaceConfigurationsForChecking(ctx, workspace, world.Env)
	require.NoError(t, err)
	require.Equal(t, []string{"platform"}, rootGroups)
}
