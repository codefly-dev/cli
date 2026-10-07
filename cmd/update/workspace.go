package update

import (
	"fmt"

	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var agentOverrideFlags []string

// WorkspaceCmd moves every workspace service to its latest compatible agent,
// or, with --agent-override, pins an agent for every composed service in the
// workspace's committed agent-overrides block.
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Update every workspace service to its latest compatible agent, or pin one with --agent-override",
	Long: `Update the agents the workspace's services run on.

Without flags every service moves to its agent's latest compatible release.

With --agent-override <publisher>/<name>=<version> nothing else moves: the entry
is written to the top-level agent-overrides block of workspace.codefly.yaml,
which moves every composed service on that agent to exactly that version —
prereleases included, the way to consume a dev build from ` + "`codefly publish dev`" + `.
Entries the command does not name are kept. Before the file is written the
version must be an exact semantic version, the key must name an agent some
composed service runs on, and the agent must be published at that version and
start with a compatible protocol; any failure leaves the file untouched.`,
	Example: `  codefly update workspace                                                         # latest releases
  codefly update workspace --agent-override codefly.dev/go-grpc=0.1.47-dev.abc123def456 # pin a dev build`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if len(agentOverrideFlags) > 0 {
			return overrideWorkspaceAgents(agentOverrideFlags)
		}
		workspace, err := common.LoadWorkspace(cmd.Context())
		if err != nil {
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		return updateWorkspace(workspace)
	},
}

func init() {
	WorkspaceCmd.Flags().StringArrayVar(&agentOverrideFlags, "agent-override", nil,
		"Pin <publisher>/<name>=<version> in workspace.codefly.yaml agent-overrides (repeatable; prereleases such as dev builds accepted) instead of moving services to their latest release")
}

func overrideWorkspaceAgents(values []string) error {
	ctx, done := common.NewContext()
	defer done()

	// Validate the spelling before touching the workspace: a malformed flag
	// must not first materialize every composed module.
	overrides, err := parseAgentOverrideFlags(values)
	if err != nil {
		return err
	}
	workspace, err := common.LoadWorkspaceWithPinnedModules(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	uses, err := pinAgentOverrides(ctx, workspace, overrides)
	if err != nil {
		return err
	}
	for i := range uses {
		cli.Info("%s", uses[i].Line())
	}
	return nil
}

func updateWorkspace(workspace *resources.Workspace) error {
	ctx, done := common.NewContext()
	defer done()

	mods, err := workspace.LoadModules(ctx)
	if err != nil {
		return fmt.Errorf("cannot load modules: %w", err)
	}
	for _, mod := range mods {
		cli.Header(1, "Updating module <%s>", mod.Name)
		svcs, err := mod.LoadServices(ctx)
		if err != nil {
			return fmt.Errorf("cannot load services for module %s: %w", mod.Name, err)
		}
		for _, svc := range svcs {
			cli.Header(2, "Updating service <%s>", svc.Name)
			update, err := updateServiceAgent(ctx, svc)
			if err != nil {
				return fmt.Errorf("cannot update service %s/%s: %w", mod.Name, svc.Name, err)
			}
			if update != nil {
				cli.Header(2, "Updating agent <%s> version: %s -> %s", update.Name, update.From, update.To)
			}
		}
	}
	return nil
}
