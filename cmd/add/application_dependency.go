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
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		if err := addApplicationDependency(ctx); err != nil {
			return fmt.Errorf("cannot add application dependency: %w", err)
		}
		return ctx.Err()
	},
}

func addApplicationDependency(ctx context.Context) error {
	workspace, mod, err := common.LoadRequiredModuleE(ctx, nil)
	if err != nil {
		return err
	}

	// Load applications in the module
	apps, err := mod.LoadApplications(ctx)
	if err != nil {
		return fmt.Errorf("cannot load applications: %w", err)
	}

	if len(apps) == 0 {
		return fmt.Errorf("no applications found in module %q", mod.Name)
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
		if err != nil {
			return fmt.Errorf("cannot select application: %w", err)
		}
		application, err = mod.LoadApplicationFromName(ctx, selected.Identifier)
		if err != nil {
			return fmt.Errorf("cannot load application: %w", err)
		}
	}

	confirm, err := models.ConfirmE(ctx, fmt.Sprintf("Confirm adding an application dependency for <%s>?", application.Name), true)
	if err != nil {
		return fmt.Errorf("cannot confirm dependency creation: %w", err)
	}
	if !confirm {
		cli.Header(2, "Received loud and clear!")
		return nil
	}

	// Choose dependency type
	depTypeEntries := []*models.Entry{
		{Identifier: "application", Description: "Another Application"},
		{Identifier: "service", Description: "A Service"},
	}
	depType, err := models.Select("What type of dependency?", depTypeEntries)
	if err != nil {
		return fmt.Errorf("cannot select dependency type: %w", err)
	}

	if depType.Identifier == "application" {
		return addAppToAppDependency(ctx, workspace, mod, application)
	}
	if depType.Identifier == "service" {
		return addAppToServiceDependency(ctx, workspace, application)
	}
	return fmt.Errorf("unsupported dependency type %q", depType.Identifier)
}

func addAppToAppDependency(ctx context.Context, workspace *resources.Workspace, mod *resources.Module, application *resources.Application) error {
	// Collect all applications from all modules
	var allApps []*resources.Application
	for _, modRef := range workspace.Modules {
		m, err := workspace.LoadModuleFromName(ctx, modRef.Name)
		if err != nil {
			return fmt.Errorf("cannot load module %q: %w", modRef.Name, err)
		}
		apps, err := m.LoadApplications(ctx)
		if err != nil {
			return fmt.Errorf("cannot load applications from module %q: %w", m.Name, err)
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
		return fmt.Errorf("no other applications found")
	}

	var entries []*models.Entry
	for _, app := range allApps {
		entries = append(entries, &models.Entry{
			Identifier:  fmt.Sprintf("%s/%s", app.Module(), app.Name),
			Description: fmt.Sprintf("%s (module: %s)", app.Name, app.Module()),
		})
	}

	selected, err := models.Select("Select the application dependency", entries)
	if err != nil {
		return fmt.Errorf("cannot select application dependency: %w", err)
	}

	// Parse the selected application
	depApp := findAppByIdentifier(allApps, selected.Identifier)
	if depApp == nil {
		return fmt.Errorf("cannot find selected application %q", selected.Identifier)
	}

	// Add the dependency
	application.AddApplicationDependency(depApp.Name, depApp.Module())
	err = application.Save(ctx)
	if err != nil {
		return fmt.Errorf("cannot save application: %w", err)
	}

	cli.Header(2, "Application dependency on %s/%s added", depApp.Module(), depApp.Name)
	return nil
}

func addAppToServiceDependency(ctx context.Context, workspace *resources.Workspace, application *resources.Application) error {
	// Collect all services from all modules
	type serviceEntry struct {
		Name   string
		Module string
	}
	var allServices []serviceEntry

	for _, modRef := range workspace.Modules {
		m, err := workspace.LoadModuleFromName(ctx, modRef.Name)
		if err != nil {
			return fmt.Errorf("cannot load module %q: %w", modRef.Name, err)
		}
		for _, svcRef := range m.ServiceReferences {
			allServices = append(allServices, serviceEntry{
				Name:   svcRef.Name,
				Module: m.Name,
			})
		}
	}

	if len(allServices) == 0 {
		return fmt.Errorf("no services found")
	}

	var entries []*models.Entry
	for _, svc := range allServices {
		entries = append(entries, &models.Entry{
			Identifier:  fmt.Sprintf("%s/%s", svc.Module, svc.Name),
			Description: fmt.Sprintf("%s (module: %s)", svc.Name, svc.Module),
		})
	}

	selected, err := models.Select("Select the service dependency", entries)
	if err != nil {
		return fmt.Errorf("cannot select service dependency: %w", err)
	}

	// Parse the selected service
	var depSvc *serviceEntry
	for i := range allServices {
		if fmt.Sprintf("%s/%s", allServices[i].Module, allServices[i].Name) == selected.Identifier {
			depSvc = &allServices[i]
			break
		}
	}
	if depSvc == nil {
		return fmt.Errorf("cannot find selected service %q", selected.Identifier)
	}

	// Add the dependency
	application.AddServiceDependency(depSvc.Name, depSvc.Module)
	err = application.Save(ctx)
	if err != nil {
		return fmt.Errorf("cannot save application: %w", err)
	}

	cli.Header(2, "Service dependency on %s/%s added", depSvc.Module, depSvc.Name)
	return nil
}

func findAppByIdentifier(apps []*resources.Application, identifier string) *resources.Application {
	for _, app := range apps {
		if fmt.Sprintf("%s/%s", app.Module(), app.Name) == identifier {
			return app
		}
	}
	return nil
}
