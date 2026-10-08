package install

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// ModulesCmd prepares selected modules without running services or rendering deployments.
var ModulesCmd = &cobra.Command{
	Use:   "modules",
	Short: "Materialize the current workspace's selected modules without running services",
	Long: `Prepare all modules selected by workspace.codefly.yaml using the same package
materializer, trust policy, local overrides and resolution receipts as codefly run.
Writes the module cache and machine-local overlay/receipts; adds ignore entries
when needed. User-managed path and worktree overrides remain selected.

Does not start agents or services, resolve secret values, build images, or render
or apply deployments. Returns an error if any selected module remains unloadable.
A successful installation is not runtime readiness or configuration validation.`,
	Example: "  codefly install modules\n  codefly doctor workspace --env staging --sources-only --json",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		if err := installWorkspaceModules(ctx, workspace); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Prepared %d selected modules; services were not started.\n", len(workspace.Modules))
		return err
	},
}

func installWorkspaceModules(ctx context.Context, workspace *resources.Workspace) error {
	if err := composition.EnsurePinnedModules(ctx, workspace); err != nil {
		return err
	}
	reloaded, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	if err != nil {
		return err
	}
	var failures []error
	for _, ref := range reloaded.Modules {
		resolution, err := reloaded.ResolveModule(ctx, ref)
		if err != nil {
			failures = append(failures, fmt.Errorf("module %s: %w", ref.Name, err))
			continue
		}
		if resolution.Kind == resources.ResolutionPinned {
			failures = append(failures, fmt.Errorf("module %s remains unmaterialized; inspect the package/trust diagnostics above", ref.Name))
			continue
		}
		if _, err := resources.LoadModuleFromDir(ctx, resolution.Dir); err != nil {
			failures = append(failures, fmt.Errorf("module %s is not loadable: %w", ref.Name, err))
		}
	}
	return errors.Join(failures...)
}
