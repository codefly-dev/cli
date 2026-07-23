package upgrade

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// WorkspaceCmd implements `codefly upgrade workspace` — runs Upgrade
// for every service in the workspace. Each service is upgraded
// sequentially so a failure in one doesn't block the rest.
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Apply semver-safe dependency upgrades to every workspace service",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		defer services.ClearAgents()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}

		all, err := loadAllServices(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot enumerate services: %w", err)
		}

		var failures []error
		for _, ms := range all {
			if err := ctx.Err(); err != nil {
				failures = append(failures, err)
				break
			}
			resp, err := upgradeService(ctx, workspace, ms.module, ms.service)
			if err != nil {
				cli.Error("upgrade %s/%s: %v", ms.module.Name, ms.service.Name, err)
				failures = append(failures, fmt.Errorf("upgrade %s/%s: %w", ms.module.Name, ms.service.Name, err))
				continue
			}
			if jsonOut {
				if err := emitJSON(resp); err != nil {
					failures = append(failures, fmt.Errorf("encode %s/%s result: %w", ms.module.Name, ms.service.Name, err))
				}
				continue
			}
			identity, idErr := ms.service.Identity()
			if idErr != nil {
				cli.Error("identity %s/%s: %v", ms.module.Name, ms.service.Name, idErr)
				failures = append(failures, fmt.Errorf("identity %s/%s: %w", ms.module.Name, ms.service.Name, idErr))
				continue
			}
			emitTable(identity, resp)
			fmt.Println()
		}
		return errors.Join(failures...)
	},
}

type moduleService struct {
	module  *resources.Module
	service *resources.Service
}

func loadAllServices(ctx context.Context, ws *resources.Workspace) ([]moduleService, error) {
	w := wool.Get(ctx).In("loadAllServices")
	modules, err := ws.LoadModules(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "load modules")
	}
	var out []moduleService
	for _, m := range modules {
		svcs, err := m.LoadServices(ctx)
		if err != nil {
			return nil, w.Wrapf(err, "load services for module %s", m.Name)
		}
		for _, s := range svcs {
			s.WithModule(m.Name)
			out = append(out, moduleService{module: m, service: s})
		}
	}
	return out, nil
}
