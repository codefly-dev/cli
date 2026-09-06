package cmd

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/environment"
	"github.com/codefly-dev/core/tui"
)

// init wires the environment import command to the doctor workspace readiness
// engine, which lives in this package. The seam avoids an import cycle: package
// cmd imports cmd/environment to register its command, so cmd/environment
// cannot import cmd back to call workspaceReadiness directly.
func init() {
	environment.PostImportValidate = func(ctx context.Context, dir, env string) {
		report := workspaceReadiness(ctx, workspaceReadinessOptions{dir: dir, env: env})
		fmt.Println(tui.RenderHeader(1, fmt.Sprintf("codefly doctor workspace --env %s", env)))
		for _, diagnostic := range report.Checks {
			printWorkspaceDiagnostic(diagnostic)
		}
		if report.Status != readinessStatusReady {
			fmt.Println(tui.RenderError(fmt.Sprintf("Workspace is NOT ready for environment %q — fix the items marked ✗ above.", env)))
			return
		}
		fmt.Println(tui.RenderInfo(fmt.Sprintf("Workspace is ready for environment %q.", env)))
	}
}
