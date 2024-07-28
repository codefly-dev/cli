package add

import (
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
	Short: "Add a service dependency",

	Run: func(cmd *cobra.Command, args []string) {
		if interactive {
			cli.GetLogger().Oops("Interactive mode not implemented yet")
		}
		addServiceDependency(args)
	},
}

func addServiceDependency(args []string) {
	ctx, done := common.NewContext()
	defer done()
	defer services.ClearAgents()

	workspace, module, service := common.Load(ctx, args)
	if module == nil {
		cli.Error("No module found")
		return

	}
	if service == nil {
		svcs, err := module.LoadServices(ctx)
		cli.ExitOnError(err, "can't load services")
		if len(svcs) == 0 {
			cli.Error("No services found")
			return
		}
		var entries []*models.Entry
		for _, p := range svcs {
			entries = append(entries, &models.Entry{
				Identifier: p.Name,
			})
		}
		selected, err := models.Select("Select the service to add the dependency", entries)
		cli.ExitOnError(err, "cannot select service")
		service, err = module.LoadServiceFromName(ctx, selected.Identifier)
		cli.ExitOnError(err, "cannot load service")
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding a service dependency for <%s>?", service.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
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
		cli.ExitOnError(err, "cannot select service dependency")

		if selected.Identifier != otherModule {
			action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
				Name:             service.Name,
				Module:           module.Name,
				DependencyModule: module.Name,
				DependencyName:   selected.Identifier,
			})
			cli.ExitOnError(err, "cannot create action")
			_, err = actions.Run(ctx, action, &actions.Space{Module: module, Workspace: workspace})
			cli.ExitOnError(err, "cannot add service dependency")
			cli.Header(2, "Service dependency added")
			return
		}
	}
	allApps := workspace.Modules
	if len(allApps) == 1 {
		cli.Error("No other module found")
		return
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
	cli.ExitOnError(err, "cannot select module")

	otherMod, err := workspace.LoadModuleFromName(ctx, selected.Identifier)
	cli.ExitOnError(err, "cannot load module")
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
	cli.ExitOnError(err, "cannot select service")

	action, err := actionsservice.NewActionAddServiceDependency(ctx, &actionsservice.AddServiceDependency{
		Name:             service.Name,
		Module:           module.Name,
		DependencyModule: otherMod.Name,
		DependencyName:   selected.Identifier,
	})
	cli.ExitOnError(err, "cannot create action")
	_, err = actions.Run(ctx, action, &actions.Space{Module: module, Workspace: workspace})
	cli.ExitOnError(err, "cannot add service dependency")
	cli.Header(2, "Service dependency added")

}
