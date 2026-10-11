package orchestration

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestBuildAndDeployLeaveRenderOwnedConfigurationsToTheRender(t *testing.T) {
	for _, mode := range []Mode{BuildMode, SyncMode, DeployMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
				confs:       []*basev0.Configuration{workspaceConfiguration("ordinary", "MODE", "provided"), workspaceConfiguration("root", "MODE", "inherited")},
				rootConfigs: []string{"root"},
			})
			world.Mode = mode
			service := &resources.Service{WorkspaceConfigurationDependencies: []string{"workload-identity", "ordinary", "module-authority"}}
			instance := &coreservices.Instance{Service: service}
			builder := &Builder{world: world, instance: instance}
			resolved, err := builder.workspaceConfigurations(t.Context(), nil)
			require.NoError(t, err)
			require.ElementsMatch(t, []string{"ordinary", "root"}, workspaceConfigurationNames(resolved))
			// Build flows also initialize runtime peers through Runner.
			runner := &Runner{world: world, instance: instance}
			resolved, err = runner.workspaceConfigurations(t.Context(), nil, &basev0.RuntimeContext{})
			require.NoError(t, err)
			require.ElementsMatch(t, []string{"ordinary", "root"}, workspaceConfigurationNames(resolved))
			require.Equal(t, []string{"workload-identity", "ordinary", "module-authority"}, service.WorkspaceConfigurationDependencies)
			service.WorkspaceConfigurationDependencies = append(service.WorkspaceConfigurationDependencies, "unprovided")
			_, err = builder.workspaceConfigurations(t.Context(), nil)
			require.ErrorContains(t, err, "no configuration found for unprovided")
		})
	}
}
