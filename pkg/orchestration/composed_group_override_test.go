package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// writeComposedWorkspaceFile writes one file under root, creating directories.
// The composed-module case needs two workspace trees side by side, which
// writeTempWorkspace (one tree, loaded on the way out) cannot express.
func writeComposedWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

// groupKeys is the keys of one group of a read, in declaration order.
func groupKeys(infos []*basev0.ConfigurationInformation, group string) []string {
	for _, info := range infos {
		if info.GetName() != group {
			continue
		}
		keys := make([]string, 0, len(info.GetConfigurationValues()))
		for _, value := range info.GetConfigurationValues() {
			keys = append(keys, value.GetKey())
		}
		return keys
	}
	return nil
}

// A consuming workspace that declares a group a composed module also provides
// overrides it PER KEY. This is a characterization test of core's reader.
//
// The behaviour is core's, in `configurations.composeModuleWorkspaceConfigurations`
// (`configurations/local_reader.go`, `composedModuleOffers.resolve`). The
// consuming workspace wins on a name conflict, so a solution can override a
// composed configuration by declaring one of the same name — but it is never
// required to redeclare every key the host brings, and it cannot quietly drop
// one:
//
//  1. a key the root does not supply keeps the module's value;
//  2. a `${profile}` requirement the root does not discharge SURVIVES the
//     override and is reported, so `Load` fails naming it;
//  3. a root value that empties a `${profile}` key is refused by name
//     (`configurations.ErrEmptyProfileValue`), and a key the module's group does
//     not declare is refused by name (`configurations.ErrUndeclaredProfileKey`);
//  4. the group STAYS composed — `ComposedBy` keeps the name and it is absent
//     from `CompositionRootWorkspaceConfigurationNames`, so a partial override
//     does not widen the group's delivery to every service of the composition.
//
// This test used to assert the opposite of all four: core replaced the group
// whole, which lost the module's defaults, discarded the `${profile}` markers
// among them (so an empty override discharged a value that must differ per
// environment — the incident the marker was added to prevent) and reclassified
// the group as the composition root's own, which this PR's resolution would then
// inject into every service. That was filed as
// [codefly-dev/core#693](https://github.com/codefly-dev/core/issues/693) with
// this reproduction, fixed in
// [core#694](https://github.com/codefly-dev/core/pull/694) and released as the
// pinned core version; the assertions below are the flip, and the interim
// operator rule in `docs/orchestration.md` ("declare every key, or none") is
// gone with them.
//
// core#694 deliberately leaves a group inherited from a composed WORKSPACE
// replacing whole — only a composed MODULE's group overlays. That is the one
// case the operator rule still covers, and `docs/orchestration.md` says so.
//
// It runs only core code, so it passes against any state of this repository and
// is not differential evidence for anything in this PR — by construction. Its
// job is to fail the day core's behaviour changes again, so the CLI's
// expectations move with core's instead of being silently left behind.
func TestAPartialRootOverrideOverlaysAComposedModuleGroupPerKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	writeComposedWorkspaceFile(t, root, "solution/workspace.codefly.yaml",
		"name: solution\nlayout: modules\nmodules:\n  - name: host\n    path: ../host\n")
	writeComposedWorkspaceFile(t, root, "host/module.codefly.yaml",
		"kind: module\nname: host\nservices: []\n")
	// The composed module's group: one key with a default, and two each profile
	// must supply its own value for.
	writeComposedWorkspaceFile(t, root, "host/configurations/local/app-config.env",
		"CONFIG_DIR=/etc/app\nCONFIG_FILE="+configurations.ProfileValueMarker+"\nCONFIG_MODE="+configurations.ProfileValueMarker+"\n")

	workspace, err := resources.LoadWorkspaceFromDir(ctx, filepath.Join(root, "solution"))
	require.NoError(t, err)
	environment := resources.LocalEnvironment()

	// The module alone: every key is there, and both requirements are reported.
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, environment)
	require.NoError(t, err)
	require.Equal(t, []string{"CONFIG_DIR", "CONFIG_FILE", "CONFIG_MODE"}, groupKeys(provided.Infos, "app-config"))
	require.Equal(t, map[string]string{"app-config": "host"}, provided.ComposedBy)
	var unsupplied []string
	for _, requirement := range provided.Unsupplied {
		unsupplied = append(unsupplied, requirement.Group+"/"+requirement.Key)
	}
	slices.Sort(unsupplied)
	require.Equal(t, []string{"app-config/CONFIG_FILE", "app-config/CONFIG_MODE"}, unsupplied,
		"the module's own group owes both values the profile must supply")

	// The consuming workspace now supplies ONE of the two required keys, under
	// the same group name.
	writeComposedWorkspaceFile(t, root, "solution/configurations/local/app-config.env",
		"CONFIG_FILE=/etc/app/solution.yaml\n")

	provided, err = configurations.ReadWorkspaceConfigurations(ctx, workspace, environment)
	require.NoError(t, err)

	// (1) the module's keys survive an override that does not mention them.
	require.Equal(t, []string{"CONFIG_DIR", "CONFIG_FILE", "CONFIG_MODE"}, groupKeys(provided.Infos, "app-config"),
		"the root's value is overlaid onto the module's group, not substituted for it")
	dir, err := resources.GetConfigurationValue(ctx, &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace, Infos: provided.Infos,
	}, "app-config", "CONFIG_DIR")
	require.NoError(t, err)
	require.Equal(t, "/etc/app", dir, "a key the root does not supply keeps the module's default")
	file, err := resources.GetConfigurationValue(ctx, &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace, Infos: provided.Infos,
	}, "app-config", "CONFIG_FILE")
	require.NoError(t, err)
	require.Equal(t, "/etc/app/solution.yaml", file, "and the key it does supply is the root's")

	// (2) the requirement the root left undischarged is still owed, by name.
	unsupplied = nil
	for _, requirement := range provided.Unsupplied {
		unsupplied = append(unsupplied, requirement.Group+"/"+requirement.Key)
	}
	slices.Sort(unsupplied)
	require.Contains(t, unsupplied, "app-config/CONFIG_MODE",
		"the ${profile} marker the root did not discharge survives the override")

	loader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	require.NoError(t, err)
	err = loader.Load(ctx, environment)
	require.Error(t, err, "Load refuses a group that still owes a value its module declared required")
	require.Contains(t, err.Error(), "app-config/CONFIG_MODE")
	require.NotContains(t, err.Error(), "app-config/CONFIG_FILE",
		"and the one the root DID supply is discharged, so only the real omission is named")

	// (4) still composed, so it is not a composition-root group and this PR's
	// resolution delivers it only to the services that declared it.
	require.Equal(t, map[string]string{"app-config": "host"}, provided.ComposedBy,
		"the group stays attributed to the module that declared it")
	require.NotContains(t, loader.CompositionRootWorkspaceConfigurationNames(), "app-config",
		"so a partial override does not widen its delivery to every service")

	// (3) the two refusals, each by its own sentinel. An empty value for a key
	// the module declares ${profile} is the incident the marker exists to
	// prevent, and it is now refused at the read.
	writeComposedWorkspaceFile(t, root, "solution/configurations/local/app-config.env", "CONFIG_FILE=\n")
	_, err = configurations.ReadWorkspaceConfigurations(ctx, workspace, environment)
	require.ErrorIs(t, err, configurations.ErrEmptyProfileValue)
	require.Contains(t, err.Error(), "app-config/CONFIG_FILE")

	// And a key the module's group does not declare: the root is overriding
	// nothing, which is a typo, not a declaration.
	writeComposedWorkspaceFile(t, root, "solution/configurations/local/app-config.env",
		"CONFIG_FILE=/etc/app/solution.yaml\nCONFIG_EXTRA=1\n")
	_, err = configurations.ReadWorkspaceConfigurations(ctx, workspace, environment)
	require.ErrorIs(t, err, configurations.ErrUndeclaredProfileKey)
	require.Contains(t, err.Error(), "app-config/CONFIG_EXTRA")
}
