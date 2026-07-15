package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/blang/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var installVersion string

// InstallCmd installs a released service agent binary into the local cache.
// It is also the supported subprocess boundary used by the stdio MCP server,
// keeping download progress away from the MCP protocol stream.
var InstallCmd = &cobra.Command{
	Use:   "install <publisher/name[:version]>",
	Short: "Install a released service agent binary",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		agent, err := parseInstallAgent(ctx, args[0], installVersion)
		if err != nil {
			return fmt.Errorf("invalid agent: %w", err)
		}
		if agent.Version == "latest" {
			_, err = manager.PinToLatestRelease(ctx, agent)
			if err != nil {
				return fmt.Errorf("cannot resolve latest agent release: %w", err)
			}
		}
		if err := manager.Download(ctx, agent); err != nil {
			return fmt.Errorf("cannot install agent: %w", err)
		}
		cli.Info("Installed agent %s", agent.Identifier())
		return nil
	},
}

func init() {
	InstallCmd.Flags().StringVar(&installVersion, "version", "", "Version to install (defaults to the version in the identifier or latest)")
}

func parseInstallAgent(ctx context.Context, specification, overrideVersion string) (*resources.Agent, error) {
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, specification)
	if err != nil {
		return nil, err
	}
	if overrideVersion != "" {
		agent.Version = overrideVersion
	}
	if !safeAgentComponent(agent.Publisher) || !safeAgentComponent(agent.Name) {
		return nil, fmt.Errorf("publisher and name may contain only letters, digits, '.', '-', and '_'")
	}
	if agent.Version != "latest" {
		version := strings.TrimPrefix(agent.Version, "v")
		parsed, err := semver.Parse(version)
		if err != nil {
			return nil, fmt.Errorf("invalid semantic version %q: %w", agent.Version, err)
		}
		agent.Version = parsed.String()
	}
	return agent, nil
}

func safeAgentComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
