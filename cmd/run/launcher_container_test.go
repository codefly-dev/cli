package run

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	solutionhost "github.com/codefly-dev/authority/libraries/solutionhost/go"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// A run that declares a host is that host's cluster on every backend: the
// delivery host reads its federation keys from the run whether its process
// starts on the machine, in a nix shell or in a container. A container run
// used to attach no launcher, so the host stopped at runtime-init with "no
// configuration found for federation" and no playground ran on that backend.
func TestTheRunHandsTheHostItsFederationOnEveryBackend(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := solutionhost.NewInMemorySigner("test", key)
	require.NoError(t, err)
	held := loadDeveloperKey
	loadDeveloperKey = func() (solutionhost.Signer, error) { return signer, nil }
	t.Cleanup(func() { loadDeveloperKey = held })

	for _, backend := range []string{resources.RuntimeContextNative, resources.RuntimeContextNix, resources.RuntimeContextContainer} {
		t.Run(backend, func(t *testing.T) {
			workspace, env := writeHostedNativeWorkspace(t)
			manifest := filepath.Join(workspace.Dir(), "modules", "model", "services", "model", resources.ServiceConfigurationName)
			body, err := os.ReadFile(manifest)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(manifest, append(body, []byte("workspace-configuration-dependencies:\n  - federation\n")...), 0o600))
			workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
			require.NoError(t, err)
			module, err := workspace.LoadModuleFromName(t.Context(), "model")
			require.NoError(t, err)
			service, err := module.LoadServiceFromName(t.Context(), "model")
			require.NoError(t, err)

			flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, env, orchestration.RunMode,
				orchestration.WithRunModuleClosure("model"), NativeAuthorityPrerequisites(env))
			require.NoError(t, err)
			flow.WithRuntimeContext(backend)
			cluster, err := newNativeLauncher(t.Context(), flow, workspace, env, "")
			require.NoError(t, err)
			require.NotNil(t, cluster, "a %s run of an environment with a host attaches the launcher", backend)
			t.Cleanup(cluster.close)

			confs, err := flow.WorkspaceConfigurationsFor(t.Context(), service)
			require.NoError(t, err)
			values := map[string]string{}
			for _, conf := range confs {
				for _, info := range conf.Infos {
					if info.Name != environments.FederationConfiguration {
						continue
					}
					for _, value := range info.ConfigurationValues {
						values[value.Key] = value.Value
					}
				}
			}
			require.Equal(t, env.Host.Coordinate, values[environments.FederationHostCoordinateKey])
			require.Equal(t, env.Host.Domain, values[environments.FederationHostOwnershipDomainsKey])
		})
	}
}
