package orchestration

import (
	"context"
	"fmt"
	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/services"
	"testing"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestLocalRuntimeServicesMatchesActualStartPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		mode                              Mode
		testMode                          agentv0.TestDependencyMode
		excludedRoot, remoteRoot, skipped bool
		want                              []string
	}{
		{"run", RunMode, 0, false, false, false, []string{"saas/frontend", "wiki/backend"}},
		{"excluded root", RunMode, 0, true, false, false, []string{"saas/frontend"}},
		{"remote root", RunMode, 0, false, true, false, []string{"saas/frontend"}},
		{"test policy unresolved", TestMode, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_UNSPECIFIED, false, false, false, nil},
		{"test none", TestMode, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE, false, false, false, nil},
		{"test dependencies", TestMode, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES, false, false, false, []string{"saas/frontend"}},
		{"test stack", TestMode, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK, false, false, false, []string{"saas/frontend", "wiki/backend"}},
		{"skipped test", TestMode, agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK, false, false, true, nil},
		{"build", BuildMode, 0, false, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flow, ctx := multiRootFlow(t, "wiki/backend")
			flow.world.Mode = tc.mode
			flow.testDependencyMode, flow.excludeRoot, flow.validationSkipped = tc.testMode, tc.excludedRoot, tc.skipped
			for _, unique := range []string{"saas/frontend", "saas/auth-gateway", "documents/documents", "wiki/backend"} {
				ref, err := resources.ParseServiceWithOptionalModule(unique)
				require.NoError(t, err)
				service, err := flow.workspace.LoadService(ctx, ref)
				require.NoError(t, err)
				flow.services = append(flow.services, service)
			}
			flow.services = append(flow.services, nil, flow.originService)
			flow.remoteServices = []*Remote{{ServiceWithModule: &resources.ServiceWithModule{Module: "saas", Name: "auth-gateway"}}}
			if tc.remoteRoot {
				flow.remoteServices = append(flow.remoteServices, &Remote{ServiceWithModule: &resources.ServiceWithModule{Module: "wiki", Name: "backend"}})
			}
			flow.excludedDependencyServices = []string{"documents/documents"}
			flow.publishRuntimeServices()
			require.Equal(t, tc.want, flow.LocalRuntimeServices())
		})
	}
	var absent *Flow
	require.Empty(t, absent.LocalRuntimeServices())
}

// Exercise manager policy resolution and actual Start dispatch, rather than
// constructing the participant snapshot from private fields alone.
func TestRuntimeParticipantsAfterManagerInitialization(t *testing.T) {
	selection := protocoltest.Install(t, "participants")[0]
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	agent, err := resources.ParseAgent(t.Context(), resources.ServiceAgent, selection)
	require.NoError(t, err)
	for _, tc := range []struct {
		policy string
		want   []string
	}{
		{"supported", nil},
		{"unsupported", nil},
		{"dependencies", []string{"app/dependency"}},
		{"stack", []string{"app/dependency", "app/subject"}},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			t.Setenv("CODEFLY_TEST_PEER_VALIDATION", tc.policy)
			decl := func(name string) string {
				return fmt.Sprintf("name: %s\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: %s\n  name: %s\n  version: %s\n", name, agent.Publisher, agent.Name, agent.Version)
			}
			workspace := writeTempWorkspace(t, map[string]string{
				"workspace.codefly.yaml":                               "name: participants\nlayout: modules\nmodules:\n  - name: app\n",
				"modules/app/module.codefly.yaml":                      "name: app\nservices:\n  - name: subject\n  - name: dependency\n  - name: dormant\n",
				"modules/app/services/subject/service.codefly.yaml":    decl("subject") + "service-dependencies:\n  - name: dependency\n    kind: runtime\n",
				"modules/app/services/dependency/service.codefly.yaml": decl("dependency"),
				"modules/app/services/dormant/service.codefly.yaml":    decl("dormant"),
			})
			module, err := workspace.LoadModuleFromName(t.Context(), "app")
			require.NoError(t, err)
			service, err := module.LoadServiceFromName(t.Context(), "subject")
			require.NoError(t, err)
			env, err := SelectEnvironment(workspace, LocalEnvironmentName)
			require.NoError(t, err)
			flow, err := NewFlow(t.Context(), workspace, module, service, env, TestMode)
			require.NoError(t, err)
			flow.WithRuntimeContext(resources.RuntimeContextNative)
			flow.WithTemporaryPorts(true)
			t.Cleanup(func() {
				require.NoError(t, flow.Stop())
				require.NoError(t, flow.Shutdown())
				for _, key := range flow.AgentCacheKeys() {
					services.ClearAgent(key)
				}
			})
			require.NoError(t, flow.InitManagers(t.Context()))
			require.ElementsMatch(t, tc.want, flow.LocalRuntimeServices())
			var starts []string
			flow.World().BeginRuntimeStart = func(_ context.Context, s *resources.Service) (*RuntimeStartAttempt, error) {
				return &RuntimeStartAttempt{Admit: func(context.Context) error { starts = append(starts, resources.WithUnique(s).Unique()); return nil }}, nil
			}
			require.NoError(t, flow.Load(t.Context()))
			require.NoError(t, flow.Start(t.Context()))
			require.ElementsMatch(t, tc.want, starts)
		})
	}
}
