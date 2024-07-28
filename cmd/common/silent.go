package common

import (
	"context"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

func WithSilence(ctx context.Context, workspace *resources.Workspace, silents []string) {
	all, err := workspace.LoadServiceWithModules(ctx)
	cli.ExitOnError(err, "Cannot load services")

	var silentServices []*resources.ServiceWithModule
	for _, s := range all {
		for _, silent := range silents {
			if strings.Contains(s.Name, silent) {
				silentServices = append(silentServices, s)
			}
		}
	}
	if len(silentServices) > 0 {
		cli.Debug("silent services: %v", silentServices)
		cli.WithSilence(silentServices)
	}
}
