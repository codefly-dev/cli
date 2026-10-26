package orchestration

import (
	"fmt"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func TestEveryModeLeavesRenderOwnedConfigurationsToTheRender(t *testing.T) {
	for _, mode := range []Mode{RunMode, CompileMode, LintMode, TestMode, BuildMode, SyncMode, DeployMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			world := loadedWorkspaceWorld(t, staticWorkspaceLoader{
				confs:       []*basev0.Configuration{workspaceConfiguration("ordinary", "MODE", "provided"), workspaceConfiguration("root", "MODE", "inherited")},
				rootConfigs: []string{"root"},
			})
			world.Mode = mode
			service := &resources.Service{WorkspaceConfigurationDependencies: []string{"ordinary", "module-authority"}}
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
			require.Equal(t, []string{"ordinary", "module-authority"}, service.WorkspaceConfigurationDependencies)
			service.WorkspaceConfigurationDependencies = append(service.WorkspaceConfigurationDependencies, "unprovided")
			_, err = builder.workspaceConfigurations(t.Context(), nil)
			require.ErrorContains(t, err, "no configuration found for unprovided")
			_, err = runner.workspaceConfigurations(t.Context(), nil, &basev0.RuntimeContext{})
			require.ErrorContains(t, err, "no configuration found for unprovided")
		})
	}
}

// Exercise Runner.Init's configuration reads against real workspace files. The
// endpointless service needs neither a listener nor an agent for these reads.
func TestRuntimeInitInputsWithoutRenderOwnedConfigurationFiles(t *testing.T) {
	for _, mode := range []Mode{RunMode, CompileMode, LintMode, TestMode} {
		t.Run(string(mode), func(t *testing.T) {
			workspace := writeTempWorkspace(t, map[string]string{
				"workspace.codefly.yaml": "name: bare\nlayout: modules\nmodules:\n    - name: payments\n",
				"modules/payments/module.codefly.yaml": "kind: module\nname: payments\n" +
					"services:\n    - name: worker\n",
				"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
					"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
					"workspace-configuration-dependencies:\n    - module-authority\n",
			})
			world, service := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = mode })
			identity, err := service.Identity()
			require.NoError(t, err)
			runner := &Runner{
				world:          world,
				instance:       &coreservices.Instance{Service: service, Identity: identity},
				runtimeContext: resources.RuntimeContextNative,
			}
			inputs, err := runner.gatherInitInputs(t.Context(), t.Context(), wool.Get(t.Context()))
			require.NoError(t, err)
			require.Empty(t, inputs.workspaceConfigurations)
			require.NoDirExists(t, filepath.Join(workspace.Dir(), "configurations"))
		})
	}
}

func TestRunNeverInheritsAuthoredRenderOwnedRootGroups(t *testing.T) {
	for _, declared := range []bool{false, true} {
		t.Run(fmt.Sprint(declared), func(t *testing.T) {
			declarations := ""
			if declared {
				declarations = "workspace-configuration-dependencies:\n    - module-authority\n"
			}
			workspace := writeTempWorkspace(t, map[string]string{
				"workspace.codefly.yaml":               "name: bare\nlayout: modules\nmodules:\n    - name: payments\n",
				"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nservices:\n    - name: worker\n",
				"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
					"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" + declarations,
				"configurations/local/module-authority.env": "PRINCIPAL=authored\n",
				"configurations/local/ordinary.env":         "MODE=inherited\n",
			})
			world, service := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = RunMode })
			require.Equal(t, []string{"ordinary"}, world.effectiveWorkspaceConfigurationGroups(service.WorkspaceConfigurationDependencies))
			runner := &Runner{world: world, instance: &coreservices.Instance{Service: service}}
			resolved, err := runner.workspaceConfigurations(t.Context(), nil, &basev0.RuntimeContext{})
			require.NoError(t, err)
			require.Equal(t, []string{"ordinary"}, workspaceConfigurationNames(resolved))
			// The loaded root groups stay intact for the render's authored-file
			// refusal; filtering delivery must not erase that evidence.
			require.Contains(t, world.compositionRootWorkspaceConfigurationGroups(), "module-authority")
		})
	}
}
