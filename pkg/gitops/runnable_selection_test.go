package gitops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestRuntimeInstallationsSelectAuthorityEnvelopeAndConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, installation string
		runtime            bool
		want               []string
	}{
		{"one", `{"contract":"runtime.installation/v1","bindings":[{"prepared":"OWNER__ITEMS_APPLY"}]}`, true, []string{preparedKey}},
		{"union", `[{"contract":"runtime.installation/v1","bindings":[{"prepared":"OWNER__ITEMS_APPLY"}]},{"contract":"runtime.installation/v1","bindings":[{"prepared":"OWNER__ADMIN"},{"prepared":"OWNER__ITEMS_APPLY"}]}]`, true, []string{"OWNER__ADMIN", preparedKey}},
		{"empty", `{"contract":"runtime.installation/v1","bindings":[]}`, true, nil},
		{"robin unchanged despite root installation", `{"contract":"runtime.installation/v1","bindings":[]}`, false, []string{"OWNER__ADMIN", preparedKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := writeInstallerWorkspace(t, true, preparedKey+"="+preparedValue(t)+"\nOWNER__ADMIN="+preparedValue(t)+"\n")
			groups := []string{runnables.RunnableBindingsGroup}
			if tc.runtime {
				groups = append(groups, runnables.RuntimeExecutionGroup)
			}
			require.NoError(t, os.WriteFile(filepath.Join(w.module.Dir(), "services/api/service.codefly.yaml"), []byte(exposedHostedServiceYAML("api", groups...)+"module-identity: true\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(w.workspace.Dir(), "configurations/test/runtime-execution.env"), []byte("INSTALLATION_JSON="+tc.installation+"\n"), 0o600))
			var err error
			w.workspace, err = resources.LoadWorkspaceFromDir(ctx, w.workspace.Dir())
			require.NoError(t, err)
			w.module, err = w.workspace.LoadModuleFromName(ctx, "shop")
			require.NoError(t, err)
			authority, err := w.authority(t)
			require.NoError(t, err)
			var got []string
			for _, binding := range authority.Bindings[len(contractBindings()):] {
				got = append(got, binding.ID)
			}
			require.ElementsMatch(t, tc.want, got)
			envelope, err := WithPreparedBindingGrants(ctx, w.workspace, w.env, map[string]json.RawMessage{"shop": json.RawMessage(`{"operation_audiences":{"OWNER__ADMIN":{"audience":"acme.items"}}}`)})
			require.NoError(t, err)
			var grant struct {
				Audiences map[string]json.RawMessage `json:"operation_audiences"`
			}
			require.NoError(t, json.Unmarshal(envelope["shop"], &grant))
			got = nil
			for key := range grant.Audiences {
				got = append(got, key)
			}
			require.ElementsMatch(t, tc.want, got)
			flow, err := orchestration.NewFlow(ctx, w.workspace, nil, nil, w.env, orchestration.SnapshotMode)
			require.NoError(t, err)
			flow.WithRuntimeContext(resources.RuntimeContextContainer)
			require.NoError(t, flow.ConfigurationManager.Load(ctx, w.env.Runtime()))
			service, err := w.module.LoadServiceFromName(ctx, "api")
			require.NoError(t, err)
			delivered, err := flow.WorkspaceConfigurationsFor(ctx, service)
			require.NoError(t, err)
			got = nil
			for _, conf := range delivered {
				for _, info := range conf.Infos {
					if info.Name == runnables.RunnableBindingsGroup {
						for _, value := range info.ConfigurationValues {
							got = append(got, value.Key)
						}
					}
				}
			}
			require.ElementsMatch(t, tc.want, got)
		})
	}
}

func TestRuntimeInstallationSelectsReachPerConsumer(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "selected"}[selected], func(t *testing.T) {
			ctx := context.Background()
			workspace := writeExecutorWorkspace(t, "rest")
			api := filepath.Join(workspace.Dir(), "modules/shop/services/api/service.codefly.yaml")
			data, err := os.ReadFile(api)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(api, append(data, []byte("workspace-configuration-dependencies:\n  - runnable-bindings\n")...), 0o600))
			worker := filepath.Join(workspace.Dir(), "modules/runtime/services/worker/service.codefly.yaml")
			data, err = os.ReadFile(worker)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(worker, append(data, []byte("  - runtime-execution\n")...), 0o600))
			dir := filepath.Join(workspace.Dir(), "configurations/staging")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			bindings := `[]`
			if selected {
				bindings = `[{"prepared":"SHOP__SHOP_REST_SYNC"}]`
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "runtime-execution.env"), []byte(`INSTALLATION_JSON={"contract":"runtime.installation/v1","bindings":`+bindings+"}\n"), 0o600))
			workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
			require.NoError(t, err)
			env := selectedEnvironment(t, workspace, "staging")
			reach, err := derivedRunnableReach(ctx, workspace, env)
			require.NoError(t, err)
			want := 1
			if selected {
				want++
			}
			require.Len(t, preparedReachConsumers(reach), want)
			require.Contains(t, reach.consumers["shop/api/rest"], runnableExecutor{Module: "shop", Service: "api"})
			if selected {
				require.Contains(t, reach.consumers["shop/api/rest"], runnableExecutor{Module: "runtime", Service: "worker"})
			}
			env.Host = &environments.EnvironmentHost{Gateway: "shop/api", TrustDomain: "example.test"}
			index, err := indexMeshComposition(ctx, workspace, env)
			require.NoError(t, err)
			require.Len(t, index.sources("shop", "api", "rest"), want)
		})
	}
}
