package common

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

func WithSilence(ctx context.Context, workspace *resources.Workspace, silent []string) {
	// Get the silent services
	var silentServices []*resources.ServiceWithModule
	for _, s := range silent {
		service, err := resources.ParseServiceWithOptionalModule(s)
		cli.ExitOnError(err, "Cannot parse silent service")
		if service.Module == "" {
			// Find unique service by name
			svc, err := workspace.FindUniqueServiceAndModuleByName(ctx, service.Name)
			cli.ExitOnError(err, "Cannot find unique service by name")
			service.Module = svc.Module
		}
		silentServices = append(silentServices, service)
	}
	if len(silentServices) > 0 {
		cli.Debug("silent services: %v", silentServices)
		cli.WithSilence(silentServices)
	}
}
