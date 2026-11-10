package add

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"

	"github.com/codefly-dev/core/actions/actions"
	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/spf13/cobra"
)

var (
	serviceDependencyTarget string
	serviceDependencyYes    bool
)

// ServiceDependencyCmd represents the run command
var ServiceDependencyCmd = &cobra.Command{
	Use:   "dependency [service]",
	Short: "Link a service to another service it requires",
	Long: `Link a service to another service it requires.

The service that gains the dependency is the positional argument, or the one
the current directory belongs to. Without --dependency the service to depend
on is chosen from a list, which needs a terminal.

Examples:
  # From the service's directory, choose the dependency from a list
  codefly add dependency

  # Without a terminal (CI, scripts, MCP): name it, in the same module...
  codefly add dependency --dependency store --yes

  # ...or in another module
  codefly add dependency billing/api --dependency accounts/store --yes
`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}

		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		defer services.ClearAgents()

		if err := addServiceDependency(ctx, args); err != nil {
			return fmt.Errorf("cannot add service dependency: %w", err)
		}
		return ctx.Err()
	},
}

func addServiceDependency(ctx context.Context, args []string) error {
	workspace, module, service, err := common.LoadRequiredE(ctx, args)
	if err != nil {
		return err
	}

	// Non-interactive mode: answer the confirmation with its default.
	cli.SetWithDefault(serviceDependencyYes)

	// A named dependency needs no list: resolve it, in the service's own
	// module unless it is qualified, and refuse what does not exist.
	if serviceDependencyTarget != "" {
		target, parseErr := resources.ParseServiceWithOptionalModule(serviceDependencyTarget)
		if parseErr != nil {
			return fmt.Errorf("--dependency is a service or module/service: %w", parseErr)
		}
		dependencyModule := module
		if target.Module != "" && target.Module != module.Name {
			dependencyModule, err = workspace.LoadModuleFromName(ctx, target.Module)
			if err != nil {
				return fmt.Errorf("cannot load dependency module %s: %w", target.Module, err)
			}
		}
		if dependencyModule.Name == module.Name && target.Name == service.Name {
			return fmt.Errorf("service %s/%s cannot depend on itself", module.Name, service.Name)
		}
		found := false
		for _, ref := range dependencyModule.ServiceReferences {
			found = found || ref.Name == target.Name
		}
		if !found {
			return fmt.Errorf("module %s has no service %s", dependencyModule.Name, target.Name)
		}
		confirm, confirmErr := models.ConfirmE(ctx, fmt.Sprintf("Confirm adding <%s/%s> as a dependency of <%s/%s>?",
			dependencyModule.Name, target.Name, module.Name, service.Name), true)
		if confirmErr != nil {
			return fmt.Errorf("cannot confirm dependency creation (pass --yes without a terminal): %w", confirmErr)
		}
		if !confirm {
			cli.Header(2, "Received loud and clear!")
			return nil
		}
		return linkServiceDependency(ctx, workspace, module, service, dependencyModule.Name, target.Name)
	}

	confirm, err := models.ConfirmE(ctx, fmt.Sprintf("Confirm adding a service dependency for <%s>?", service.Name), true)
	if err != nil {
		return fmt.Errorf("cannot confirm dependency creation: %w", err)
	}
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		return nil
	}

	// First all services in the same module
	modServices := module.ServiceReferences

	// only 1 service means must be in another mod
	if len(modServices) > 1 {
		var entries []*models.Entry
		for _, p := range modServices {
			if p.Name == service.Name {
				continue
			}
			entries = append(entries, &models.Entry{
				Identifier: p.Name,
			})
		}
		// Or another module
		otherModule := ">> In another module"
		entries = append(entries, &models.Entry{
			Identifier: otherModule,
		})
		selected, selectedErr := models.Select("Select the dependency or >> In another module", entries)
		if selectedErr != nil {
			return fmt.Errorf("cannot select service dependency (name it with --dependency without a terminal): %w", selectedErr)
		}

		if selected.Identifier != otherModule {
			return linkServiceDependency(ctx, workspace, module, service, module.Name, selected.Identifier)
		}
	}
	allApps := workspace.Modules
	if len(allApps) == 1 {
		return fmt.Errorf("no other module found")
	}
	var entries []*models.Entry
	for _, p := range allApps {
		if p.Name == module.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier: p.Name,
		})
	}
	selected, err := models.Select("Select the module for the dependent service", entries)
	if err != nil {
		return fmt.Errorf("cannot select dependency module (name it with --dependency without a terminal): %w", err)
	}

	otherMod, err := workspace.LoadModuleFromName(ctx, selected.Identifier)
	if err != nil {
		return fmt.Errorf("cannot load dependency module: %w", err)
	}
	entries = []*models.Entry{}
	for _, p := range otherMod.ServiceReferences {
		if p.Name == service.Name {
			continue
		}
		entries = append(entries, &models.Entry{
			Identifier: p.Name,
		})
	}
	selected, err = models.Select("Select the dependent service", entries)
	if err != nil {
		return fmt.Errorf("cannot select dependent service: %w", err)
	}

	return linkServiceDependency(ctx, workspace, module, service, otherMod.Name, selected.Identifier)
}

// linkServiceDependency records that service requires dependencyModule's
// dependencyName, through the same action whichever way it was chosen.
func linkServiceDependency(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, dependencyModule, dependencyName string) error {
	action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
		Name:             service.Name,
		Module:           module.Name,
		DependencyModule: dependencyModule,
		DependencyName:   dependencyName,
	})
	if err != nil {
		return fmt.Errorf("cannot create dependency action: %w", err)
	}
	_, err = actions.Run(ctx, action, &actions.Space{Module: module, Workspace: workspace})
	if err != nil {
		return fmt.Errorf("cannot run dependency action: %w", err)
	}
	cli.Header(2, "Service dependency added")
	return nil
}

func init() {
	ServiceDependencyCmd.Flags().StringVar(&serviceDependencyTarget, "dependency", "",
		"Service to depend on: a service of the same module, or module/service")
	ServiceDependencyCmd.Flags().BoolVar(&serviceDependencyYes, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
