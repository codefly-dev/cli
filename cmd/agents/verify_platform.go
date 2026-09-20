package agents

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// VerifyPlatformCmd checks artifacts for caller-selected agents. It makes no
// compatibility claim; protocol compatibility is checked on the running peer.
var VerifyPlatformCmd = &cobra.Command{
	Use:   "verify-platform <os_arch> <publisher/name>...",
	Short: "Verify selected agents ship a release asset for a platform",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		var selected []*resources.Agent
		for _, spec := range args[1:] {
			agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, spec)
			if err != nil {
				return err
			}
			selected = append(selected, agent)
		}
		return verifyAgentPlatform(ctx, normalizePlatform(args[0]), selected)
	},
}

// normalizePlatform accepts either the docker "linux/arm64" spelling or the
// release-asset "linux_arm64" spelling and returns the asset spelling, which is
// how a release's shipped platforms are recorded.
func normalizePlatform(platform string) string {
	return strings.ReplaceAll(strings.TrimSpace(platform), "/", "_")
}

func verifyAgentPlatform(ctx context.Context, platform string, selected []*resources.Agent) error {
	if len(selected) == 0 {
		return fmt.Errorf("at least one agent selection is required")
	}
	var missing []string
	for _, agent := range selected {
		ok, version, shipped := platformResolvable(ctx, agent, platform)
		if ok {
			cli.Info("%s/%s@%s ships %s", agent.Publisher, agent.Name, version, platform)
			continue
		}
		missing = append(missing, fmt.Sprintf("%s/%s@%s ships [%s]", agent.Publisher, agent.Name, dashIfEmpty(version), strings.Join(shipped, " ")))
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d language agent(s) have no %s release asset — a %s companion cannot resolve them:\n  %s",
			len(missing), platform, platform, strings.Join(missing, "\n  "))
	}
	cli.Info("all %d language agents resolve for %s", len(selected), platform)
	return nil
}

// platformResolvable reports whether the agent's latest resolvable release ships
// a downloadable asset for the target os_arch, reusing the same release
// inventory as `agent versions`. It returns that version and the platforms it
// ships so a gap reads at a glance.
func platformResolvable(ctx context.Context, agent *resources.Agent, platform string) (ok bool, version string, shipped []string) {
	inv := collectInventory(ctx, agent, nil)
	selectedVersion := agent.Version
	if selectedVersion == "" || selectedVersion == latestAgentVersion {
		selectedVersion = inv.LatestResolvable
	}
	if selectedVersion == "" {
		return false, "", nil
	}
	for _, entry := range inv.Versions {
		if entry.Version == selectedVersion {
			return slices.Contains(entry.ReleasePlatforms, platform), entry.Version, entry.ReleasePlatforms
		}
	}
	return false, selectedVersion, nil
}
