package run

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// WorkspaceCmd runs the whole composition the way a cell runs it: the host's
// ingress services and every solution's service-entry are the roots, and the
// dependency closure of those roots is every module they consume. It is what
// `codefly run` means inside a composition workspace, and the local loop a
// platform is proven on before it is rendered for a cell.
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Start the whole composition locally: the host's ingress services and every solution, with every module they reach",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			done()
			return fmt.Errorf("cannot load workspace: %w", err)
		}
		if err = composition.MaterializePinnedModules(ctx, workspace); err != nil {
			done()
			return err
		}
		workspace, err = common.LoadWorkspace(ctx)
		if err != nil {
			done()
			return fmt.Errorf("cannot reload workspace: %w", err)
		}
		roots, err := CompositionRoots(ctx, workspace)
		done()
		if err != nil {
			return err
		}
		pinsAlreadyResolved = true
		defer func() { pinsAlreadyResolved = false }()
		return runServiceCommand(cmd, roots)
	},
}

// CompositionRoots is the root set of a composition run: the host's ingress
// services of the selected environment first (a person reaches the platform
// through them), then every module that declares a service-entry — each
// solution. Every other service is reached through what these consume.
func CompositionRoots(ctx context.Context, workspace *resources.Workspace) ([]string, error) {
	env, err := orchestration.SelectEnvironment(workspace, environmentName)
	if err != nil {
		return nil, err
	}
	var roots []string
	seen := map[string]bool{}
	add := func(unique string) {
		if !seen[unique] {
			seen[unique] = true
			roots = append(roots, unique)
		}
	}
	if env.Host != nil {
		host := strings.SplitN(env.Host.Delivery, "/", 2)[0]
		for _, route := range env.Ingress {
			add(resources.ServiceUnique(host, route.Service))
		}
	}
	for _, ref := range workspace.Modules {
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref.Name, err)
		}
		if mod.ServiceEntry != "" {
			add(resources.ServiceUnique(mod.Name, mod.ServiceEntry))
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("workspace <%s> composes nothing to run: its environment %q declares no host ingress and no module declares a service-entry", workspace.Name, environmentName)
	}
	return roots, nil
}

func init() {
	WorkspaceCmd.Flags().StringVar(&fixture, "fixture", "", "Fixture override (defaults to the selected Codefly environment)")
	WorkspaceCmd.Flags().StringVar(&environmentName, "env", orchestration.LocalEnvironmentName, "Workspace environment to run")
	WorkspaceCmd.Flags().BoolVar(&headless, "headless", false, "Run without TUI (auto-enabled when no TTY, e.g. MCP, CI, pipes)")
	WorkspaceCmd.Flags().DurationVar(&readinessTimeout, "readiness-timeout", 10*time.Minute, "Maximum time to wait for the flow to become ready")
	WorkspaceCmd.Flags().StringVar(&profile, "profile", "", "Named workspace run profile")
	WorkspaceCmd.Flags().StringSliceVar(&excludeDependencies, "exclude-dependency", nil, "Exclude optional dependency services from the run (repeatable, e.g. infra/temporal)")
	WorkspaceCmd.Flags().StringSliceVar(&setOverrides, "set", nil, "Per-service runtime env override (repeatable)")
	WorkspaceCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	WorkspaceCmd.Flags().StringVar(&runtimeContext, "runtime-context", defaultRuntimeContext(), "Runtime context for the flow (native/container/nix/free)")
	WorkspaceCmd.Flags().StringVar(&namingScope, "naming-scope", "", NamingScopeUsage)
	WorkspaceCmd.Flags().BoolVar(&withCLIServer, "cli-server", false, "Start CLI server")
	WorkspaceCmd.Flags().BoolVar(&startDocker, "start-docker", true, "Auto-start a local Docker engine if a service needs Docker and it isn't running")
}
