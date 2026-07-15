package common

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	resources "github.com/codefly-dev/core/resources"
)

// WithSilenceE configures service-output suppression without exiting, so
// callers can still tear down partially initialized agents on failure.
func WithSilenceE(ctx context.Context, workspace *resources.Workspace, silents []string) error {
	if workspace == nil {
		return fmt.Errorf("workspace is required")
	}
	all, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return fmt.Errorf("cannot load services: %w", err)
	}

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
	return nil
}
