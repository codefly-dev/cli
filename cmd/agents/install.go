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

var (
	installVersion string
	installKind    string
)

// InstallCmd installs a released agent binary into the local cache.
// It is also the supported subprocess boundary used by the stdio MCP server,
// keeping download progress away from the MCP protocol stream.
var InstallCmd = &cobra.Command{
	Use:   "install <publisher/name[:version]>",
	Short: "Download a released agent into the local Codefly cache",
	Long: `Download a released agent binary into the local Codefly cache.

--kind selects which agent kind to install; a runnable language agent is
installed by its language name, exactly like a service agent.

Examples:
  codefly agent install go-grpc:0.1.4
  codefly agent install python:0.0.1 --kind=runnable`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		agent, err := parseInstallAgent(ctx, args[0], installVersion, installKind)
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
	InstallCmd.Flags().StringVar(&installKind, "kind", "service", "Agent kind to install (service, runnable, …)")
}

func parseInstallAgent(ctx context.Context, specification, overrideVersion, kind string) (*resources.Agent, error) {
	agentKind, err := resolveAgentKind(kind)
	if err != nil {
		return nil, err
	}
	agent, err := resources.ParseAgent(ctx, agentKind, specification)
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

// resolveAgentKind turns the short kind a user types into the registered agent
// kind. Both the short form (`runnable`) and the registered form
// (`codefly:runnable`) are accepted; the registry decides which exist.
func resolveAgentKind(kind string) (resources.AgentKind, error) {
	if kind == "" {
		return resources.ServiceAgent, nil
	}
	candidate := resources.AgentKind(kind)
	if !strings.Contains(kind, ":") {
		candidate = resources.AgentKind("codefly:" + kind)
	}
	if _, err := resources.AgentKindRegistrationFor(candidate); err != nil {
		return "", fmt.Errorf("unknown agent kind %q: %w", kind, err)
	}
	return candidate, nil
}
