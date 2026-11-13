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
	deploy.WorkspaceReadiness = func(ctx context.Context, question deploy.ReadinessQuestion) (deploy.ReadinessVerdict, error) {
		env, module := question.Env, question.Module
		report := workspaceReadiness(ctx, &workspaceReadinessOptions{
			env:     env,
			module:  module,
			timeout: deployReadinessTimeout,

			// A render ships what its pins name; it refuses a pin that is
			// behind its source unless a hold states why. A publication of
			// a render that judged them is held to that judgement instead.
			requireCurrentPins:  question.JudgePins,
			pinsJudgedElsewhere: !question.JudgePins,
		})
		verdict := deploy.ReadinessVerdict{PinsJudged: question.JudgePins, Behind: pinsBehind(report)}
		if report.Status == readinessStatusReady {
			// Held pins are deliberate, and a render is where they must be seen.
			for i := range report.Checks {
				if report.Checks[i].Code == codePinHeld {
					printWorkspaceDiagnostic(&report.Checks[i])
				}
			}
			return verdict, nil
		}
		fmt.Println(tui.RenderHeader(1, fmt.Sprintf("codefly doctor workspace --env %s --module %s", env, module)))
		for i := range report.Checks {
			printWorkspaceDiagnostic(&report.Checks[i])
		}
		// The banner is a refusal, so only the verb that refuses prints it. Under
		// the operator's override the verdict is still reported — header and
		// diagnostics above — and the caller warns that the next verb will refuse;
		// printing "Workspace is NOT ready" on a run that then succeeds and exits 0
		// would make every log scraper read a failure where there was none.
		if question.Enforcing {
			fmt.Println(tui.RenderError(fmt.Sprintf("Workspace is NOT ready for environment %q — fix the items marked ✗ above.", env)))
		}
		return verdict, fmt.Errorf("workspace is not ready for environment %q: %s", env, firstReadinessFailure(report))
	}
}

// pinsBehind names every pin the report found behind its source, or could
// not compare with it, that no hold covers: what a render under the
// override records, and what its publish then refuses.
func pinsBehind(report *workspaceReadinessReport) []string {
	var behind []string
	for _, diagnostic := range report.Checks {
		if diagnostic.Code == codePinStale || diagnostic.Code == codePinCurrencyUnverified {
			behind = append(behind, diagnostic.Message)
		}
	}
	return behind
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
