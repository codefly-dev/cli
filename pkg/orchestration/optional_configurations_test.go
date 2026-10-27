package orchestration

import (
	"context"
	"errors"
	"testing"

	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// An environment that prepares no runnable binding has no runnable-bindings
// group, and that absence is the answer: every mode delivers nothing for the
// group to a service that declares it, through the run's and the render's
// shared resolution, while a provided group is delivered as before and any
// other unprovided group is still refused.
func TestEveryModeDeliversNothingForAnUnprovidedOptionalGroup(t *testing.T) {
	for _, mode := range []Mode{RunMode, CompileMode, LintMode, TestMode, BuildMode, SyncMode, DeployMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			resolve := func(t *testing.T, confs ...*basev0.Configuration) ([]string, []string, *resources.Service) {
				t.Helper()
				world := loadedWorkspaceWorld(t, staticWorkspaceLoader{confs: confs})
				world.Mode = mode
				service := &resources.Service{WorkspaceConfigurationDependencies: []string{"ordinary", runnablespkg.RunnableBindingsGroup}}
				instance := &coreservices.Instance{Service: service}
				built, err := (&Builder{world: world, instance: instance}).workspaceConfigurations(t.Context(), nil)
				require.NoError(t, err)
				ran, err := (&Runner{world: world, instance: instance}).workspaceConfigurations(t.Context(), nil, &basev0.RuntimeContext{})
				require.NoError(t, err)
				return workspaceConfigurationNames(built), workspaceConfigurationNames(ran), service
			}
			ordinary := workspaceConfiguration("ordinary", "MODE", "provided")
			built, ran, service := resolve(t, ordinary)
			require.Equal(t, []string{"ordinary"}, built)
			require.Equal(t, []string{"ordinary"}, ran)
			require.Equal(t, []string{"ordinary", runnablespkg.RunnableBindingsGroup}, service.WorkspaceConfigurationDependencies)

			built, ran, _ = resolve(t, ordinary, workspaceConfiguration(runnablespkg.RunnableBindingsGroup, "APP__WORKER_OPERATION", "{}"))
			require.ElementsMatch(t, []string{"ordinary", runnablespkg.RunnableBindingsGroup}, built)
			require.ElementsMatch(t, []string{"ordinary", runnablespkg.RunnableBindingsGroup}, ran)

			world := loadedWorkspaceWorld(t, staticWorkspaceLoader{})
			world.Mode = mode
			_, err := world.workspaceConfigurationsFor(t.Context(), &resources.Service{WorkspaceConfigurationDependencies: []string{"ordinary", runnablespkg.RunnableBindingsGroup}}, nil, resources.NewNativeNetworkAccess())
			require.ErrorContains(t, err, "no configuration found for ordinary")
		})
	}
}

// A service that declares the group composes and initializes against real
// workspace files when the workspace holds no runnable-bindings file at all.
func TestRuntimeInitInputsWithoutARunnableBindingsFile(t *testing.T) {
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":               "name: bare\nlayout: modules\nmodules:\n    - name: payments\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nservices:\n    - name: worker\n",
		"configurations/local/ordinary.env":    "MODE=provided\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - ordinary\n    - " + runnablespkg.RunnableBindingsGroup + "\n",
	})
	world, service := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = RunMode })
	identity, err := service.Identity()
	require.NoError(t, err)
	runner := &Runner{
		world:          world,
		instance:       &coreservices.Instance{Service: service, Identity: identity},
		runtimeContext: resources.RuntimeContextNative,
	}
	inputs, err := runner.gatherInitInputs(t.Context(), t.Context(), wool.Get(t.Context()))
	require.NoError(t, err)
	require.Equal(t, []string{"ordinary"}, workspaceConfigurationNames(inputs.workspaceConfigurations))
}

// ambiguousWorkspaceLoader is a static loader that also reports groups two
// composed modules define differently, as core's local reader does.
type ambiguousWorkspaceLoader struct {
	staticWorkspaceLoader
	ambiguous map[string]error
}

func (l ambiguousWorkspaceLoader) AmbiguousWorkspaceConfigurations() map[string]error {
	return l.ambiguous
}

// An optional group two composed modules define differently is not an absent
// one: it still reaches core, which refuses it naming the conflict.
func TestAnAmbiguousOptionalGroupIsStillRefused(t *testing.T) {
	ctx := context.Background()
	loader := ambiguousWorkspaceLoader{ambiguous: map[string]error{
		runnablespkg.RunnableBindingsGroup: errors.Join(configurations.ErrConfigurationConflict, errors.New("defined differently by module-a and module-b")),
	}}
	workspace := writeTempWorkspace(t, map[string]string{"workspace.codefly.yaml": "name: bare\nlayout: flat\n"})
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	manager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	manager.WithLoader(loader)
	require.NoError(t, manager.Load(ctx, env.Runtime()))
	world := &World{
		ConfigurationManager:  manager,
		compositionRootGroups: loader.CompositionRootWorkspaceConfigurationNames,
		providedWorkspaceConfigurationInfos: func() []*basev0.ConfigurationInformation {
			return workspaceConfigurationInfos(loader.Configurations())
		},
		ambiguousWorkspaceConfigurations: loader.AmbiguousWorkspaceConfigurations,
	}
	_, err = world.workspaceConfigurationsFor(ctx, &resources.Service{WorkspaceConfigurationDependencies: []string{runnablespkg.RunnableBindingsGroup}}, nil, resources.NewNativeNetworkAccess())
	require.ErrorIs(t, err, configurations.ErrConfigurationConflict)
}
