package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/codefly-dev/core/tui"
)

// deployReadinessTimeout bounds the gate the same way `codefly doctor
// workspace` bounds itself: secret resolution reaches a provider and must not
// hang a delivery verb.
const deployReadinessTimeout = 30 * time.Second

// init wires the GitOps delivery verbs to the doctor workspace readiness
// engine, which lives in this package. The seam avoids an import cycle: package
// cmd imports cmd/deploy to register its commands, so cmd/deploy cannot import
// cmd back to call workspaceReadiness directly. It is the same seam
// `environment.PostImportValidate` uses, for the same reason.
func init() {
	deploy.WorkspaceReadiness = func(ctx context.Context, env, module string) error {
		report := workspaceReadiness(ctx, workspaceReadinessOptions{
			env:     env,
			module:  module,
			timeout: deployReadinessTimeout,
		})
		if report.Status == readinessStatusReady {
			return nil
		}
		fmt.Println(tui.RenderHeader(1, fmt.Sprintf("codefly doctor workspace --env %s --module %s", env, module)))
		for _, diagnostic := range report.Checks {
			printWorkspaceDiagnostic(diagnostic)
		}
		fmt.Println(tui.RenderError(fmt.Sprintf("Workspace is NOT ready for environment %q — fix the items marked ✗ above.", env)))
		return fmt.Errorf("workspace is not ready for environment %q: %s", env, firstReadinessFailure(report))
	}
}

// firstReadinessFailure carries the doctor's own message and fix hint into the
// returned error, so the refusal is legible in a log that only keeps the error
// — a CI job's last line — and not only on the terminal that printed the report.
func firstReadinessFailure(report *workspaceReadinessReport) string {
	for _, diagnostic := range report.Checks {
		if diagnostic.Status != checkStatusFail {
			continue
		}
		parts := []string{fmt.Sprintf("%s [%s]", diagnostic.Message, diagnostic.Code)}
		if diagnostic.Remediation != "" {
			parts = append(parts, "fix: "+diagnostic.Remediation)
		}
		return strings.Join(parts, " — ")
	}
	return "see the diagnostics above"
}
