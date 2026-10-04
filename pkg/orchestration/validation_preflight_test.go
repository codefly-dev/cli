package orchestration

import (
	"fmt"
	"os"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func TestUnsupportedValidationNeverInitializesRuntimeOrDependencies(t *testing.T) {
	selection := protocoltest.Install(t, "validation")[0]
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	t.Setenv("CODEFLY_TEST_PEER_VALIDATION", "unsupported")
	t.Setenv("CODEFLY_TEST_PEER_INIT_ERROR", "required configuration is absent")
	for _, mode := range []Mode{LintMode, CompileMode, TestMode} {
		t.Run(string(mode), func(t *testing.T) {
			flow := validationPreflightFlow(t, selection, mode, true)
			require.NoError(t, flow.InitManagers(t.Context()))
			require.NoError(t, flow.Load(t.Context()))
			require.NoError(t, flow.Start(t.Context()))
			require.True(t, flow.OriginValidationSkipped())
			require.Equal(t, mode == TestMode, flow.OriginTestSkipped())
			require.NoError(t, flow.Stop())
			require.NoError(t, flow.Shutdown(), "disposal must not send Destroy to an uninitialized runtime")
			require.Equal(t, []string{"app/subject"}, flow.AgentCacheKeys())
			for _, key := range flow.AgentCacheKeys() {
				services.ClearAgent(key)
			}
		})
	}
	for _, call := range protocoltest.Calls(t, os.Getenv("CODEFLY_TEST_PEER_ROOT")) {
		require.Equal(t, "Agent.GetAgentInformation", call.Method, "no runtime lifecycle or operation may run for an unsupported validation")
	}
}

func TestSupportedAndLegacyValidationStillRequireRuntimeConfiguration(t *testing.T) {
	selection := protocoltest.Install(t, "validation")[0]
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	t.Setenv("CODEFLY_TEST_PEER_INIT_ERROR", "required configuration is absent")
	for _, advertisement := range []string{"supported", ""} {
		for _, mode := range []Mode{LintMode, CompileMode, TestMode} {
			t.Run(advertisement+"/"+string(mode), func(t *testing.T) {
				t.Setenv("CODEFLY_TEST_PEER_VALIDATION", advertisement)
				flow := validationPreflightFlow(t, selection, mode, false)
				defer func() {
					_ = flow.Stop()
					_ = flow.Shutdown()
					for _, key := range flow.AgentCacheKeys() {
						services.ClearAgent(key)
					}
				}()
				require.NoError(t, flow.InitManagers(t.Context()))
				require.False(t, flow.OriginValidationSkipped())
				require.NoError(t, flow.Load(t.Context()))
				require.ErrorContains(t, flow.Start(t.Context()), "required configuration is absent")
			})
		}
	}
}

func validationPreflightFlow(t *testing.T, selection string, mode Mode, dependency bool) *Flow {
	t.Helper()
	agent, err := resources.ParseAgent(t.Context(), resources.ServiceAgent, selection)
	require.NoError(t, err)
	declaration := fmt.Sprintf("name: subject\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: %s\n  name: %s\n  version: %s\n", agent.Publisher, agent.Name, agent.Version)
	if dependency {
		declaration += "service-dependencies:\n  - name: prerequisite\n    kind: runtime\n"
	}
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                                 "name: validation\nlayout: modules\nmodules:\n  - name: app\n",
		"modules/app/module.codefly.yaml":                        "name: app\nservices:\n  - name: subject\n  - name: prerequisite\n",
		"modules/app/services/subject/service.codefly.yaml":      declaration,
		"modules/app/services/prerequisite/service.codefly.yaml": "name: prerequisite\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: example.test\n  name: missing-prerequisite\n  version: 0.0.1\n",
	})
	module, err := workspace.LoadModuleFromName(t.Context(), "app")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "subject")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	flow, err := NewFlow(t.Context(), workspace, module, service, env, mode)
	require.NoError(t, err)
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithTemporaryPorts(true)
	return flow
}
