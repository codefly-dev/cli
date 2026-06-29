package add

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/resources"

	"github.com/spf13/cobra"
)

// ApplicationDependencyCmd represents the add application dependency command
var ApplicationDependencyCmd = &cobra.Command{
	Use:   "application-dependency",
	Short: "Add an application dependency",

	Run: func(cmd *cobra.Command, args []string) {
		if interactive {
			cli.GetLogger().Oops("Interactive mode not implemented yet")
		}
		addApplicationDependency(args)
	},
}

func addApplicationDependency(args []string) {
	ctx, done := common.NewContext()
	defer done()

	workspace := common.RequireWorkspace(ctx)
	mod := common.RequireModule(ctx)

	// Load applications in the module
	apps, err := mod.LoadApplications(ctx)
	cli.ExitOnError(err, "can't load applications")

	if len(apps) == 0 {
		cli.Error("No applications found in module")
		return
	}

	// Select the application to add dependency to
	var appEntries []*models.Entry
	for _, app := range apps {
		appEntries = append(appEntries, &models.Entry{
			Identifier: app.Name,
		})
	}

	var application *resources.Application
	if len(appEntries) == 1 {
		application = apps[0]
	} else {
		selected, err := models.Select("Select the application to add the dependency to", appEntries)
		cli.ExitOnError(err, "cannot select application")
		application, err = mod.LoadApplicationFromName(ctx, selected.Identifier)
		cli.ExitOnError(err, "cannot load application")
	}

	confirm := models.Confirm(ctx, fmt.Sprintf("Confirm adding an application dependency for <%s>?", application.Name), true)
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		cli.Exit()
	}

	// Choose dependency type
	depTypeEntries := []*models.Entry{
		{Identifier: "application", Description: "Another Application"},
		{Identifier: "service", Description: "A Service"},
	}
	depType, err := models.Select("What type of dependency?", depTypeEntries)
	cli.ExitOnError(err, "cannot select dependency type")

	if depType.Identifier == "application" {
		addAppToAppDependency(ctx, workspace, mod, application)
	} else {
		addAppToServiceDependency(ctx, workspace, mod, application)
	}
}

func addAppToAppDependency(ctx context.Context, workspace *resources.Workspace, mod *resources.Module, application *resources.Application) {
	// Collect all applications from all modules
	var allApps []*resources.Application
	for _, modRef := range workspace.Modules {
		m, err := workspace.LoadModuleFromName(ctx, modRef.Name)
		if err != nil {
			continue
		}
		apps, err := m.LoadApplications(ctx)
		if err != nil {
			continue
		}
		for _, app := range apps {
			// Don't include self
			if app.Name == application.Name && m.Name == mod.Name {
				continue
			}
			app.SetModule(m.Name)
			allApps = append(allApps, app)
		}
	}

	if len(allApps) == 0 {
		cli.Error("No other applications found")
		return
	}

	var entries []*models.Entry
	for _, app := range allApps {
		entries = append(entries, &models.Entry{
			Identifier:  fmt.Sprintf("%s/%s", app.Module(), app.Name),
			Description: fmt.Sprintf("%s (module: %s)", app.Name, app.Module()),
		})
	}

	selected, err := models.Select("Select the application dependency", entries)
	cli.ExitOnError(err, "cannot select application")

	// Parse the selected application
	depApp := findAppByIdentifier(allApps, selected.Identifier)
	if depApp == nil {
		cli.Error("Cannot find selected application")
		return
	}

	// Add the dependency
	application.AddApplicationDependency(depApp.Name, depApp.Module())
	err = application.Save(ctx)
	cli.ExitOnError(err, "cannot save application")

	cli.Header(2, "Application dependency on %s/%s added", depApp.Module(), depApp.Name)
}

func addAppToServiceDependency(ctx context.Context, workspace *resources.Workspace, mod *resources.Module, application *resources.Application) {
	// Collect all services from all modules
	type serviceEntry struct {
		Name   string
		Module string
	}
	var allServices []serviceEntry

	for _, modRef := range workspace.Modules {
		m, err := workspace.LoadModuleFromName(ctx, modRef.Name)
		if err != nil {
			continue
		}
		for _, svcRef := range m.ServiceReferences {
			allServices = append(allServices, serviceEntry{
				Name:   svcRef.Name,
				Module: m.Name,
			})
		}
	}

	if len(allServices) == 0 {
		cli.Error("No services found")
		return
	}

	var entries []*models.Entry
	for _, svc := range allServices {
		entries = append(entries, &models.Entry{
			Identifier:  fmt.Sprintf("%s/%s", svc.Module, svc.Name),
			Description: fmt.Sprintf("%s (module: %s)", svc.Name, svc.Module),
		})
	}

	selected, err := models.Select("Select the service dependency", entries)
	cli.ExitOnError(err, "cannot select service")

	// Parse the selected service
	var depSvc *serviceEntry
	for _, svc := range allServices {
		if fmt.Sprintf("%s/%s", svc.Module, svc.Name) == selected.Identifier {
			depSvc = &svc
			break
		}
	}
	if depSvc == nil {
		cli.Error("Cannot find selected service")
		return
	}

	// Add the dependency
	application.AddServiceDependency(depSvc.Name, depSvc.Module)
	err = application.Save(ctx)
	cli.ExitOnError(err, "cannot save application")

	cli.Header(2, "Service dependency on %s/%s added", depSvc.Module, depSvc.Name)
}

func findAppByIdentifier(apps []*resources.Application, identifier string) *resources.Application {
	for _, app := range apps {
		if fmt.Sprintf("%s/%s", app.Module(), app.Name) == identifier {
			return app
		}
	}
	return nil
}
