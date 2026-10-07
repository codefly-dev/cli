package update

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var serviceAgentVersion string

// ServiceCmd moves one service's agent: to its latest compatible release, or,
// with --agent-version, to exactly the version given — a release or a
// prerelease such as a `codefly publish dev` build.
var ServiceCmd = &cobra.Command{
	Use:   "service [<service>]",
	Short: "Update a service to its latest compatible agent, or pin one with --agent-version",
	Long: `Update a service's agent in its own service.codefly.yaml.

Without --agent-version the service moves to its agent's latest compatible
release. With --agent-version it is pinned to exactly that version, prereleases
included — the way to consume a dev build from ` + "`codefly publish dev`" + `.
The version must be an exact semantic version, and the candidate is downloaded
and its protocol checked before the file is written, so a version that was never
published is refused and the file is left untouched.

Only the agent.version token is rewritten; comments, formatting and every other
key are kept byte-for-byte. A service of a composed module is refused: its
service.codefly.yaml is that module's content. Move its agent with
` + "`codefly update workspace --agent-override <publisher>/<name>=<version>`" + ` instead.`,
	Example: `  codefly update service api                                         # latest compatible release
  codefly update service api --agent-version 0.1.47-dev.abc123def456 # pin a dev build`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, mod, service, err := common.LoadRequiredE(ctx, args)
		if err != nil {
			return fmt.Errorf("cannot load service: %w", err)
		}
		if err = refuseComposedService(workspace, mod, service); err != nil {
			return err
		}
		return updateService(ctx, service, serviceAgentVersion)
	},
}

func init() {
	ServiceCmd.Flags().StringVar(&serviceAgentVersion, "agent-version", "",
		"Pin the service's agent to exactly this version (prereleases such as dev builds accepted) instead of its latest release")
}

// refuseComposedService refuses to write the agent of a service whose module the
// workspace composes by source: that module's files are its published content,
// and the committed way to move its agent is the workspace's agent-overrides.
func refuseComposedService(workspace *resources.Workspace, mod *resources.Module, service *resources.Service) error {
	if workspace == nil || mod == nil {
		return nil
	}
	for _, ref := range workspace.Modules {
		if ref.Name != mod.Name || ref.Source == "" {
			continue
		}
		agent := "<publisher>/<name>"
		if service.Agent != nil {
			agent = service.Agent.Publisher + "/" + service.Agent.Name
		}
		return fmt.Errorf("service %s belongs to module %s, composed from %s: its %s is that module's content, not this workspace's; move its agent with `codefly update workspace --agent-override %s=<version>`",
			service.Name, mod.Name, ref.Source, resources.ServiceConfigurationName, agent)
	}
	return nil
}

func updateService(ctx context.Context, service *resources.Service, version string) error {
	cli.Header(2, "Updating service <%s>", service.Name)
	var update *agentUpdate
	var err error
	if version != "" {
		update, err = pinServiceAgent(ctx, service, version)
	} else {
		update, err = updateServiceAgent(ctx, service)
	}
	if err != nil {
		return fmt.Errorf("cannot update service %s: %w", service.Name, err)
	}
	if update != nil {
		cli.Header(2, "Updating agent <%s> version: %s -> %s", update.Name, update.From, update.To)
	}
	return nil
}
