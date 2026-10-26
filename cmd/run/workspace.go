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
		pinsAlreadyResolved, workspaceRun = true, true
		defer func() { pinsAlreadyResolved, workspaceRun = false, false }()
		return runServiceCommand(cmd, roots)
	},
}

// CompositionRoots is the root set of a composition run: the host's ingress
// services of the selected environment first (a person reaches the platform
// through them), then every service of every other composed module — each
// solution, and each module a solution or another module reaches.
//
// Every service, not every service-entry. What one module needs of another is
// not only its declared service dependencies: a solution's api.consumes and a
// module's configuration references (`${endpoint:model/model/rest}`) name
// producers the dependency graph never sees, and a run that starts only what
// the graph reaches comes up beside nothing those name. The composition is
// what declares the platform, so the platform is what it composes. A run
// profile's exclusions still apply.
//
// The host module is the one exception: its ingress services are the roots,
// and the rest of it is reached through their dependencies, as a person would
// reach it.
func CompositionRoots(ctx context.Context, workspace *resources.Workspace) ([]string, error) {
	env, err := orchestration.SelectEnvironment(workspace, environmentName)
	if err != nil {
		return nil, err
	}
	// The exclusions only, read as declared. The run validates the profile
	// against the composition when it starts; here a name that matches
	// nothing simply excludes nothing.
	excluded := map[string]bool{}
	for _, unique := range excludeDependencies {
		excluded[strings.TrimSpace(unique)] = true
	}
	if name := strings.TrimSpace(profile); name != "" {
		declared, ok := workspace.RunProfiles[name]
		if !ok {
			return nil, fmt.Errorf("run profile %q is not declared", name)
		}
		for _, unique := range declared.ExcludeDependencies {
			excluded[strings.TrimSpace(unique)] = true
		}
	}
	var roots []string
	seen := map[string]bool{}
	add := func(unique string) {
		// An exclusion names a service the run must not start, whichever way
		// the run would have reached it.
		if excluded[unique] {
			return
		}
		if !seen[unique] {
			seen[unique] = true
			roots = append(roots, unique)
		}
	}
	host := ""
	if env.Host != nil {
		host = strings.SplitN(env.Host.Delivery, "/", 2)[0]
		for _, route := range env.Ingress {
			add(resources.ServiceUnique(host, route.Service))
		}
	}
	for _, ref := range workspace.Modules {
		if ref.Name == host {
			continue
		}
		mod, err := workspace.LoadModuleFromReference(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref.Name, err)
		}
		if mod.ServiceEntry != "" {
			add(resources.ServiceUnique(mod.Name, mod.ServiceEntry))
		}
		for _, service := range mod.ServiceReferences {
			add(resources.ServiceUnique(mod.Name, service.Name))
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("workspace <%s> composes nothing to run: its environment %q declares no host ingress and it composes no other module", workspace.Name, environmentName)
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
	WorkspaceCmd.Flags().StringSliceVar(&excludeWorkspaceConfigurations, "exclude-workspace-configuration", nil, "Exclude a workspace configuration group from the run, with the producers its values name (repeatable)")
	WorkspaceCmd.Flags().StringSliceVar(&setOverrides, "set", nil, "Per-service runtime env override (repeatable)")
	WorkspaceCmd.Flags().StringSliceVar(&silent, "silent", nil, "Silence services in CLI output")
	WorkspaceCmd.Flags().StringVar(&runtimeContext, "runtime-context", defaultRuntimeContext(), "Runtime context for the flow (native/container/nix/free)")
	WorkspaceCmd.Flags().StringVar(&namingScope, "naming-scope", "", NamingScopeUsage)
	WorkspaceCmd.Flags().BoolVar(&temporaryPorts, "temporary-ports", false, TemporaryPortsUsage)
	WorkspaceCmd.Flags().StringVar(&outputEnv, "output-env", "", "Write one service's full SDK/runtime environment to an owner-only file")
	WorkspaceCmd.Flags().StringVar(&outputEnvService, "output-env-service", "", "Service whose runtime environment to export (module/service; defaults to the first composition root)")
	WorkspaceCmd.Flags().BoolVar(&withCLIServer, "cli-server", false, "Start CLI server")
	WorkspaceCmd.Flags().BoolVar(&startDocker, "start-docker", true, "Auto-start a local Docker engine if a service needs Docker and it isn't running")
}
