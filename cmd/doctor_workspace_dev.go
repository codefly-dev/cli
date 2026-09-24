package cmd

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
)

// codeGitOpsDevDeploymentActive reports a rendered environment carrying a dev
// deployment (`codefly deploy dev`): it runs code no release describes.
const codeGitOpsDevDeploymentActive = "gitops_dev_deployment_active"

// checkDevDeployments warns about every dev deployment recorded in the
// workspace's rendered module trees. It reads only the render inventories.
func checkDevDeployments(ws *resources.Workspace, report *workspaceReadinessReport) {
	inventories, err := filepath.Glob(filepath.Join(ws.Dir(), "deployments", "modules", "*", gitops.InventoryFilename))
	if err != nil {
		return
	}
	sort.Strings(inventories)
	for _, path := range inventories {
		// An unreadable inventory is the render's own concern; this check only
		// surfaces dev deployments a readable one records.
		inventory, loadErr := gitops.LoadInventory(filepath.Dir(path))
		if loadErr != nil {
			continue
		}
		for i := range inventory.Dev {
			dev := &inventory.Dev[i]
			revision := dev.Commit
			if revision == "" {
				revision = "untracked"
			}
			if dev.Dirty {
				revision += " (dirty)"
			}
			report.add(codeGitOpsDevDeploymentActive,
				fmt.Sprintf("dev deployment %s/%s in %s", inventory.Module, dev.Service, inventory.Environment), "warn",
				fmt.Sprintf("environment %s runs service %s/%s from %s at %s (image %s, deployed %s): code no release describes",
					inventory.Environment, inventory.Module, dev.Service, dev.Source, revision, dev.Image, dev.DeployedAt.Format("2006-01-02T15:04:05Z")),
				fmt.Sprintf("leave dev mode with a full render: `codefly deploy gitops render %s --env %s`", inventory.Module, inventory.Environment))
		}
	}
}
