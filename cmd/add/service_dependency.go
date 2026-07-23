package add

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/services"

	"github.com/codefly-dev/core/actions/actions"
	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/spf13/cobra"
)

// ServiceDependencyCmd represents the run command
var ServiceDependencyCmd = &cobra.Command{
	Use:   "dependency",
	Short: "Link a service to another service it requires",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
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
		selected, err := models.Select("Select the dependency or >> In another module", entries)
		if err != nil {
			return fmt.Errorf("cannot select service dependency: %w", err)
		}

		if selected.Identifier != otherModule {
			action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
				Name:             service.Name,
				Module:           module.Name,
				DependencyModule: module.Name,
				DependencyName:   selected.Identifier,
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
		return fmt.Errorf("cannot select dependency module: %w", err)
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

	action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
		Name:             service.Name,
		Module:           module.Name,
		DependencyModule: otherMod.Name,
		DependencyName:   selected.Identifier,
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
