package audit

import (
	"context"
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// WorkspaceCmd implements `codefly audit workspace`.
//
// Walks every service in the workspace, runs each agent's Audit RPC,
// prints a per-service section, and (if --fail-on-vuln) exits non-zero
// when any service surfaces HIGH/CRITICAL findings.
var WorkspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Audit every service in the workspace",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()
		defer services.ClearAgents()

		workspace, err := resources.FindWorkspaceUp(ctx)
		cli.ExitOnError(err, "cannot find workspace")
		if workspace == nil {
			cli.ExitWithMessage("no workspace found")
		}

		all, err := loadAllServices(ctx, workspace)
		cli.ExitOnError(err, "cannot enumerate services")

		anyHighSeverity := false
		anyError := false
		for _, ms := range all {
			resp, err := auditService(ctx, workspace, ms.module, ms.service)
			if err != nil {
				cli.Error("audit %s/%s: %v", ms.module.Name, ms.service.Name, err)
				anyError = true
				continue
			}
			if jsonOut {
				emitJSON(resp)
			} else {
				identity, idErr := ms.service.Identity()
				if idErr != nil {
					cli.Error("identity %s/%s: %v", ms.module.Name, ms.service.Name, idErr)
					anyError = true
					continue
				}
				emitTable(identity, resp)
				fmt.Println()
			}
			if hasHighSeverity(resp) {
				anyHighSeverity = true
			}
		}

		if failOnVuln && (anyHighSeverity || anyError) {
			os.Exit(1)
		}
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

func init() {
	// (Flag definitions are shared via service.go — they're package-level vars.)
}
