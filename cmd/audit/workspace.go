package audit

import (
	"context"
	"fmt"

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
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		defer services.ClearAgents()

		workspace, err := resources.FindWorkspaceUp(ctx)
		if err != nil {
			return fmt.Errorf("cannot find workspace: %w", err)
		}
		if workspace == nil {
			return fmt.Errorf("no workspace found")
		}

		all, err := loadAllServices(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot enumerate services: %w", err)
		}

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

		if err := workspaceAuditResult(anyError, failOnVuln, anyHighSeverity); err != nil {
			return err
		}
		return nil
	},
}

func workspaceAuditResult(anyError, failOnVuln, anyHighSeverity bool) error {
	if anyError {
		return fmt.Errorf("workspace audit incomplete because one or more services failed")
	}
	if failOnVuln && anyHighSeverity {
		return fmt.Errorf("workspace audit found HIGH or CRITICAL vulnerabilities")
	}
	return nil
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
	// The audit flags are package-level vars, but cobra flags must be
	// registered on EACH command that accepts them — service.go only binds
	// them to ServiceCmd. Without these, `audit workspace --fail-on-vuln`
	// errored ("unknown flag") and failOnVuln was always false, so the
	// workspace CI gate could never exit non-zero.
	WorkspaceCmd.Flags().BoolVar(&jsonOut, "json", false, "Emit raw JSON instead of a table")
	WorkspaceCmd.Flags().BoolVar(&includeOutdated, "outdated", true, "Also report outdated patch+minor releases")
	WorkspaceCmd.Flags().BoolVar(&failOnVuln, "fail-on-vuln", false, "Exit non-zero if any HIGH/CRITICAL finding is present")
}
