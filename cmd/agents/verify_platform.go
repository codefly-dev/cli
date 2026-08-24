package agents

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// VerifyPlatformCmd asserts that every language service agent — the source-
// workspace compatibility roster is the CLI's canonical list of them — ships a
// downloadable release asset for a target platform in its latest resolvable
// release.
//
// It guards the arm64 companion footgun: `codefly companion build/publish` on an
// Apple Silicon host defaults to linux/arm64 (dockerArch() → arm64), and a
// companion built for an arch whose language agents publish no asset can't run a
// single language service. Running this for linux_arm64 in CI turns a regression
// — an agent that drops the arch from its releases — into a loud failure here
// instead of an opaque 404 at a runtime pull.
//
// It checks the latest resolvable release, not the roster's pinned version: the
// pin is a source-workspace compatibility value promoted through a separate
// qualification gate (agent promote-source), so the arm64 signal that matters
// for a companion is whether each agent is still shipping the arch at all.
var VerifyPlatformCmd = &cobra.Command{
	Use:   "verify-platform <os_arch>",
	Short: "Verify every language agent ships a release asset for a platform (e.g. linux_arm64)",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		return verifyRosterPlatform(ctx, normalizePlatform(args[0]))
	},
}

// normalizePlatform accepts either the docker "linux/arm64" spelling or the
// release-asset "linux_arm64" spelling and returns the asset spelling, which is
// how a release's shipped platforms are recorded.
func normalizePlatform(platform string) string {
	return strings.ReplaceAll(strings.TrimSpace(platform), "/", "_")
}

func verifyRosterPlatform(ctx context.Context, platform string) error {
	plugins := sourceworkspace.Roster().Plugins
	var missing []string
	for _, plugin := range plugins {
		agent := plugin.Agent()
		ok, version, shipped := platformResolvable(ctx, agent, platform)
		if ok {
			cli.Info("%s/%s@%s ships %s", plugin.Publisher, plugin.Name, version, platform)
			continue
		}
		missing = append(missing, fmt.Sprintf("%s/%s@%s ships [%s]", plugin.Publisher, plugin.Name, dashIfEmpty(version), strings.Join(shipped, " ")))
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d language agent(s) have no %s release asset — a %s companion cannot resolve them:\n  %s",
			len(missing), platform, platform, strings.Join(missing, "\n  "))
	}
	cli.Info("all %d language agents resolve for %s", len(plugins), platform)
	return nil
}

// platformResolvable reports whether the agent's latest resolvable release ships
// a downloadable asset for the target os_arch, reusing the same release
// inventory as `agent versions`. It returns that version and the platforms it
// ships so a gap reads at a glance.
func platformResolvable(ctx context.Context, agent *resources.Agent, platform string) (ok bool, version string, shipped []string) {
	inv := collectInventory(ctx, agent, nil)
	if inv.LatestResolvable == "" {
		return false, "", nil
	}
	for _, entry := range inv.Versions {
		if entry.Version == inv.LatestResolvable {
			return slices.Contains(entry.ReleasePlatforms, platform), entry.Version, entry.ReleasePlatforms
		}
	}
	return false, inv.LatestResolvable, nil
}
