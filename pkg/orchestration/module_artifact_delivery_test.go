package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// boundWorkspace is a composition whose selected profile binds one public
// module artifact to a workspace destination: the shape Product would author
// for the reviewed Documents installation. The artifact lives in the composed
// module, selected by an explicit committed path.
func boundWorkspace(t *testing.T, body string, extra map[string]string) *resources.Workspace {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml": `name: artifact-delivery
layout: modules
modules:
    - name: wiki
      path: modules/wiki
`,
		"modules/wiki/module.codefly.yaml":                   excludedRootExportModule,
		"modules/wiki/services/backend/service.codefly.yaml": excludedRootExportService + "workspace-configuration-dependencies:\n    - docs\n",
		"modules/wiki/docs/install.md":                       body,
		"configurations/local/module-artifacts.codefly.json": `{
  "schemaVersion": 1,
  "bindings": [
    {"configuration": "docs", "key": "README", "source": {"module": "wiki", "path": "docs/install.md"}}
  ]
}`,
	}
	for name, content := range extra {
		files[name] = content
	}
	return writeTempWorkspace(t, files)
}

// loadBound reads the workspace configurations the way a run and a check both
// do — through the CLI's own reader, which is where the resolver is bound.
func loadBound(t *testing.T, workspace *resources.Workspace) []*basev0.Configuration {
	t.Helper()
	reader, err := NewWorkspaceConfigurationReader(context.Background(), workspace)
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	require.NoError(t, reader.Load(context.Background(), env.Runtime()))
	return reader.Configurations()
}

// boundValue finds the delivered value for the bound destination.
func boundValue(confs []*basev0.Configuration) *basev0.ConfigurationValue {
	for _, conf := range confs {
		for _, info := range conf.GetInfos() {
			if info.GetName() != "docs" {
				continue
			}
			for _, value := range info.GetConfigurationValues() {
				if value.GetKey() == "README" {
					return value
				}
			}
		}
	}
	return nil
}

// TestAnOrdinaryLoadDeliversTheBoundArtifactWithItsEvidence is the seam end to
// end: the CLI's reader supplies the resolver, core reads the declaration
// during profile loading, acquires the bytes once and attaches the identity it
// computed FROM those bytes.
func TestAnOrdinaryLoadDeliversTheBoundArtifactWithItsEvidence(t *testing.T) {
	body := "# Install\n\nRun `codefly run`.\n"
	workspace := boundWorkspace(t, body, nil)

	value := boundValue(loadBound(t, workspace))
	require.NotNil(t, value, "the bound destination was delivered")
	require.Equal(t, body, value.GetValue(), "exact bytes, newlines included")
	require.Nil(t, value.GetModuleArtifact(), "the declaration is resolved, not delivered unresolved")
	evidence := value.GetResolvedModuleArtifact()
	require.NotNil(t, evidence)
	require.Equal(t, "wiki", evidence.GetReference().GetModule())
	require.Equal(t, "docs/install.md", evidence.GetReference().GetPath())
	require.Equal(t, int64(len(body)), evidence.GetArtifactIdentity().GetSizeBytes())
	require.NoError(t, resources.ValidateModuleArtifactValue(value))
}

// TestANextLoadDeliversChangedSource is the per-load contract, and the reason
// there is no flow-spanning freeze: an earlier observation describes itself,
// and the next supported load delivers what the source says then.
func TestANextLoadDeliversChangedSource(t *testing.T) {
	workspace := boundWorkspace(t, "A\n", nil)
	require.Equal(t, "A\n", boundValue(loadBound(t, workspace)).GetValue())

	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "modules/wiki/docs/install.md"), []byte("B\n"), 0o600))
	later := boundValue(loadBound(t, workspace))
	require.Equal(t, "B\n", later.GetValue(), "the next load acquires the then-selected source")
	require.NoError(t, resources.ValidateModuleArtifactValue(later))
}

// TestALiteralCannotSilentlyTakeTheBoundDestination is the transition rule:
// moving from a literal to a binding is an explicit reviewed source edit, so
// the two declarations together refuse rather than one quietly winning —
// whichever way precedence happens to fall.
func TestALiteralCannotSilentlyTakeTheBoundDestination(t *testing.T) {
	workspace := boundWorkspace(t, "# Install\n", map[string]string{
		"configurations/local/docs.env": "README=a literal nobody removed\n",
	})
	reader, err := NewWorkspaceConfigurationReader(context.Background(), workspace)
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	require.Error(t, reader.Load(context.Background(), env.Runtime()))
}

// TestAMissingArtifactRefusesTheLoad keeps the failure closed: there is no
// cached literal, per-key fallback or empty success for a declared binding
// whose source is gone.
func TestAMissingArtifactRefusesTheLoad(t *testing.T) {
	workspace := boundWorkspace(t, "# Install\n", nil)
	require.NoError(t, os.Remove(filepath.Join(workspace.Dir(), "modules/wiki/docs/install.md")))

	reader, err := NewWorkspaceConfigurationReader(context.Background(), workspace)
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	err = reader.Load(context.Background(), env.Runtime())
	require.Error(t, err)
	require.Contains(t, err.Error(), "wiki", "the refusal names the declared module")
	require.Contains(t, err.Error(), "docs/install.md", "and its declared path")
}

// TestACheckLoadAcquiresItsOwnObservation is the standalone-check rule: it
// uses the same loader and resolver as a run, and records what IT saw — it
// neither inherits a run's acquisition nor proves a later delivery.
func TestACheckLoadAcquiresItsOwnObservation(t *testing.T) {
	workspace := boundWorkspace(t, "A\n", nil)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	checked, _, err := WorkspaceConfigurationsForChecking(context.Background(), workspace, env)
	require.NoError(t, err)
	value := boundValue([]*basev0.Configuration{{Origin: resources.ConfigurationWorkspace, Infos: checked.Infos}})
	require.NotNil(t, value)
	require.Equal(t, "A\n", value.GetValue())
	require.NotNil(t, value.GetResolvedModuleArtifact())
}

// TestAnAdditionalOfferCannotTakeABoundDestination is the delivery check at
// the phase seams, exercised through the call the runner and the excluded-root
// export both make. Equality gives no replacement right, an empty override map
// does not skip the configuration-to-configuration comparison, and an
// unrelated offer is untouched.
func TestAnAdditionalOfferCannotTakeABoundDestination(t *testing.T) {
	workspace := boundWorkspace(t, "# Install\n", nil)
	delivered := detachConfigurations(loadBound(t, workspace))
	require.NotNil(t, boundValue(delivered))

	offer := func(key, text string) []*basev0.Configuration {
		return []*basev0.Configuration{{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
			Name: "docs", ConfigurationValues: []*basev0.ConfigurationValue{{Key: key, Value: text}},
		}}}}
	}

	require.NoError(t, resources.ValidateModuleArtifactDelivery(delivered, nil, nil),
		"the single legitimate bound value, with no contribution, passes")
	require.Error(t, resources.ValidateModuleArtifactDelivery(delivered, offer("README", "something else"), nil),
		"a returned or dependency offer for the bound destination refuses with an empty override map")
	require.Error(t, resources.ValidateModuleArtifactDelivery(delivered, offer("README", "# Install\n"), nil),
		"and copying the bytes buys no replacement authority")
	require.NoError(t, resources.ValidateModuleArtifactDelivery(delivered, offer("CHANGELOG", "unrelated"), nil),
		"an unrelated key keeps its ordinary behaviour")

	// The override map is compared by carrier, not by value, including the
	// hyphen and secret-namespace spellings the SDK accepts.
	key := resources.ConfigurationValueEnvironmentKey(
		&basev0.Configuration{Origin: resources.ConfigurationWorkspace},
		"docs", &basev0.ConfigurationValue{Key: "README"})
	require.Error(t, resources.ValidateModuleArtifactDelivery(delivered, nil, map[string]string{key: "via --set"}),
		"a runtime override for the bound carrier refuses")
	require.NoError(t, resources.ValidateModuleArtifactDelivery(delivered, nil, map[string]string{"CODEFLY__UNRELATED": "fine"}))
}

// TestTheExcludedRootExportChecksItsOwnPayload closes the one export that
// runs no agent Init: there is no returned contribution and no retained
// delivery, so the selected payload is checked against itself and against the
// export's own override map before the first append.
func TestTheExcludedRootExportChecksItsOwnPayload(t *testing.T) {
	ctx := context.Background()
	workspace := boundWorkspace(t, "# Install\n", nil)
	module, err := workspace.LoadModuleFromName(ctx, "wiki")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "backend")
	require.NoError(t, err)
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)

	flow, err := NewFlow(ctx, workspace, module, service, env, RunMode)
	require.NoError(t, err)
	output := filepath.Join(t.TempDir(), "runtime.env")
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithExcludeRoot(true)
	flow.WithOutputEnv(output)
	// An override aimed at the bound carrier: the export must refuse before it
	// writes anything at all.
	flow.WithOverrides(map[string]map[string]string{
		resources.WithUnique(service).Unique(): {
			resources.ConfigurationValueEnvironmentKey(
				&basev0.Configuration{Origin: resources.ConfigurationWorkspace},
				"docs", &basev0.ConfigurationValue{Key: "README"}): "substituted",
		},
	})
	require.Error(t, flow.exportExcludedOriginEnvironment(ctx))
	_, statErr := os.Stat(output)
	require.True(t, os.IsNotExist(statErr), "an absent output stays absent")
}
