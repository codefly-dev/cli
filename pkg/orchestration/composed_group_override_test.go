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
// REPLACES the module's whole group. This is a characterization test of core's
// reader, not an endorsement of what it pins.
//
// The behaviour is core's, in `configurations.composeModuleWorkspaceConfigurations`
// (core v0.7.1 `configurations/local_reader.go`): its `offer` closure skips a
// composed module's information outright when `fromWorkspace[info.Name]` holds,
// so the module's keys never reach any object this repository sees. The
// requirements follow the same path — `unsuppliedByGroup` is keyed by group, and
// its own comment says a group the consuming workspace overrides "owes nothing".
//
// Three consequences, all asserted below:
//
//  1. The module's default for a key the root did not supply is discarded.
//  2. The module's `${profile}` requirement for a key the root did not supply is
//     discarded too, so nothing reports it and `Load` succeeds. A root supplying
//     an EMPTY value for such a key is accepted for the same reason — which
//     defeats the marker whose whole purpose is to refuse an empty value for a
//     key that must differ per environment.
//  3. The group stops being composed, so it is reclassified as a
//     composition-root group — and this PR's resolution therefore injects the
//     truncated group into EVERY service of the composition rather than only
//     the ones that declared it. A partial override thus narrows the group's
//     contents and widens its delivery at once.
//
// Filed as codefly-dev/core#693, with this reproduction and the three losses,
// and FIXED in codefly-dev/core#694 (open and green, branch
// issue-693-fix-configurations-a-workspace-override-of-a-composed, cut from
// v0.7.1 — so the release carrying it is the next tag after v0.7.1).
//
// This test will fail the moment that release is pinned here, which is what it
// was written to do. The invariant to flip it to, over this same fixture:
//
//   - CONFIG_DIR keeps the module's /etc/app — a key the root does not supply
//     is no longer discarded;
//   - CONFIG_MODE's undischarged ${profile} survives the override and is
//     reported in Unsupplied, so Load FAILS naming app-config/CONFIG_MODE where
//     it returns nil below;
//   - a root value that empties a ${profile} key is refused by name
//     (configurations.ErrEmptyProfileValue), and a key the module's group does
//     not declare is refused by name (configurations.ErrUndeclaredProfileKey);
//   - the group STAYS composed: ComposedBy keeps the name and it is absent from
//     CompositionRootWorkspaceConfigurationNames, so consequence (3) below
//     disappears — a partial override no longer widens the truncated group's
//     delivery to every service of the composition.
//
// core#694 leaves a group inherited from a composed WORKSPACE replacing whole,
// so the operator rule in docs/orchestration.md narrows to that case rather
// than going away.
//
// What it must become, and where: core should overlay the consuming workspace's
// values onto the module's group per key, the way `profileOverlay.add` already
// does across profile derivation layers in `configurations/profile.go` — keys
// the root does not supply keep the module's value, a `${profile}` marker
// survives an override that does not discharge it, and a root value that empties
// a required key is refused by name. The semantics already exist one function
// away; they are simply not applied across the workspace/module boundary.
//
// This repository cannot settle it. By the time the CLI's shared resolution runs,
// `Loader.Configurations()` has already handed it the replaced group: the
// module's `CONFIG_DIR` and `CONFIG_MODE` exist in no object the CLI holds. The
// only way to overlay here would be to re-walk every composed module's tree and
// re-implement core's composition rule beside it, which is the second source of
// truth AGENTS.md forbids. So this test exists to fail — loudly, naming the core
// function and core#693 — on the day core changes the behaviour, so the CLI's own
// expectations are updated with it rather than silently left behind.
// It runs only core code, so it passes against any state of this repository and
// is not differential evidence for anything in this PR — by construction. Its
// job is to fail the day core's behaviour changes, so the CLI's expectations
// move with core's instead of being silently left behind.
func TestAPartialRootOverrideReplacesAComposedModuleGroupWhole(t *testing.T) {
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

	// (1) and (2): the group is the root's file and nothing else.
	require.Equal(t, []string{"CONFIG_FILE"}, groupKeys(provided.Infos, "app-config"),
		"core replaced the module's group whole; CONFIG_DIR's default and CONFIG_MODE's requirement are gone")
	require.Empty(t, provided.Unsupplied,
		"the discarded ${profile} marker is owed by nobody, so nothing refuses the missing CONFIG_MODE")

	loader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, loader.Load(ctx, environment),
		"Load accepts a group that lost a value its module declared as required")

	// (3): no longer composed, so it is a composition-root group — which this
	// PR's resolution injects into every service of the composition.
	require.Empty(t, provided.ComposedBy, "the group is no longer attributed to the module that declared it")
	require.Contains(t, loader.CompositionRootWorkspaceConfigurationNames(), "app-config",
		"a partial override widens the truncated group's delivery to every service")

	// The empty-value case the marker exists to catch, and does not.
	writeComposedWorkspaceFile(t, root, "solution/configurations/local/app-config.env", "CONFIG_FILE=\n")
	provided, err = configurations.ReadWorkspaceConfigurations(ctx, workspace, environment)
	require.NoError(t, err)
	value, err := resources.GetConfigurationValue(ctx, &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace, Infos: provided.Infos,
	}, "app-config", "CONFIG_FILE")
	require.NoError(t, err)
	require.Empty(t, value, "the root's empty value stands")
	require.Empty(t, provided.Unsupplied)
	loader, err = configurations.NewConfigurationLocalReader(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, loader.Load(ctx, environment),
		"a root override that empties a key the module declared ${profile} is accepted, which is the incident the marker was added to prevent")
}
