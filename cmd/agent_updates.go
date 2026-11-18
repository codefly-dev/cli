package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/update"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/spf13/cobra"
)

var agentUpdates string

func init() {
	RootCmd.PersistentFlags().StringVar(&agentUpdates, "agent-updates", "", "Agent freshness policy for workspace run/build/deploy: check (default), auto, require-current, off; overrides CODEFLY_AGENT_UPDATES and workspace agent-update-policy")
}

func agentUpdateCommand(cmd *cobra.Command) (bool, bool) {
	path := strings.TrimPrefix(cmd.CommandPath(), "codefly ")
	switch path {
	case "run service", "run workspace", "run job", "run command", "build service", "build module":
		return true, false
	case "deploy service", "deploy module", "deploy gitops render":
		return true, true
	}
	return false, false
}

func checkAgentUpdates(cmd *cobra.Command) error {
	supported, deploying := agentUpdateCommand(cmd)
	if !supported {
		return nil
	}
	policy := agentUpdates
	if policy == "" {
		policy = os.Getenv("CODEFLY_AGENT_UPDATES")
	}
	if policy != "" {
		if err := update.ValidatePolicy(policy, deploying); err != nil {
			return err
		}
		if policy == "off" {
			return nil
		}
	}
	// Local development binaries are intentionally not compared with releases.
	if os.Getenv(manager.AgentSourceEnv) == "local" {
		if policy == "auto" || policy == "require-current" {
			return fmt.Errorf("agent freshness policy %s requires published agents; incompatible with --local-agents", policy)
		}
		return nil
	}
	workspace, err := common.LoadWorkspaceWithPinnedModules(cmd.Context())
	if err != nil {
		return err
	}
	if policy == "" {
		if value, ok := workspace.Extensions["agent-update-policy"]; ok {
			if err = value.Decode(&policy); err != nil {
				return fmt.Errorf("agent-update-policy must be a string: %w", err)
			}
		}
	}
	if policy == "" {
		policy = "check"
	}
	if err = update.ValidatePolicy(policy, deploying); err != nil {
		return err
	}
	err = update.Preflight(cmd.Context(), workspace, policy, cmd.ErrOrStderr())
	if err != nil && policy == "check" {
		fmt.Fprintf(cmd.ErrOrStderr(), "Agent update check unavailable (freshness unknown): %v\n", err)
		return nil
	}
	return err
}
