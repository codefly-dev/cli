package ci

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/integrity"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

var (
	runSelection SelectionFlags
	runPhases    []string
)

var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run the complete Codefly-native CI gate for affected services",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.EqualFold(strings.TrimSpace(ciReportFormat), "json") {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
		}
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		cli.Init()
		defer services.ClearAgents()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		if err := common.WithSilenceE(ctx, workspace, silent); err != nil {
			return fmt.Errorf("cannot configure silent services: %w", err)
		}
		plan, err := runSelection.BuildPlan(ctx, workspace)
		if err != nil {
			return fmt.Errorf("cannot build affected-service plan: %w", err)
		}

		phases, err := normalizeRunPhases(runPhases)
		if err != nil {
			return err
		}
		return runWithCIReport(ctx, workspace, plan, "codefly ci run", func(reporter *CIReporter) error {
			if err := validateAgentVersions(ctx, workspace, plan); err != nil {
				return err
			}
			suites := normalizeTestSuites(testSuites)
			for _, phase := range phases {
				if phase == "verify" {
					if _, err := reporter.registerWorkspaceTask(ctx, workspace, phase); err != nil {
						return fmt.Errorf("prepare CI phase %s report: %w", phase, err)
					}
					continue
				}
				if phase == "test" {
					for _, suite := range suites {
						options := commandScheduleOptions(true, phase, suite, reporter)
						if err := prepareCIReportTasks(ctx, workspace, plan, options); err != nil {
							return fmt.Errorf("prepare CI phase %s report: %w", phase, err)
						}
					}
					continue
				}
				options := commandScheduleOptions(phaseLocksDependencyClosure(phase), phase, "", reporter)
				if err := prepareCIReportTasks(ctx, workspace, plan, options); err != nil {
					return fmt.Errorf("prepare CI phase %s report: %w", phase, err)
				}
			}

			return runCIPhases(ctx, phases, ciFailFast, func(phaseContext context.Context, phase string) error {
				cli.Header(2, "CI phase: %s", phase)
				return executeCIPhase(phaseContext, reporter, workspace, plan, phase, suites, ciFailFast)
			})
		})
	},
}

// runCIPhases executes every requested phase in order. When failFast is
// disabled a phase failure is recorded but does not abort the gate, so every
// remaining phase still runs and contributes its own diagnostic evidence to the
// report. Context cancellation always stops the gate, regardless of failFast.
func runCIPhases(ctx context.Context, phases []string, failFast bool, execute func(context.Context, string) error) error {
	var runErr error
	for _, phase := range phases {
		phaseErr := execute(ctx, phase)
		if phaseErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("CI phase %s failed: %w", phase, phaseErr))
			if failFast {
				return runErr
			}
		}
		// Stop the gate when the run was cancelled, even if the phase itself
		// returned nil. Avoid re-joining ctx.Err() when the phase already
		// surfaced the cancellation so the report error is not duplicated.
		if ctx.Err() != nil {
			if !errors.Is(phaseErr, ctx.Err()) {
				runErr = errors.Join(runErr, ctx.Err())
			}
			return runErr
		}
	}
	return runErr
}

// executeCIPhase runs a single phase. The test phase fans out over its named
// suites, applying the same fail-fast continuation across suites that
// runCIPhases applies across phases.
func executeCIPhase(ctx context.Context, reporter *CIReporter, workspace *resources.Workspace, plan *Plan, phase string, suites []string, failFast bool) error {
	switch phase {
	case "verify":
		return runReportedWorkspacePhase(ctx, reporter, workspace, phase, func(taskContext context.Context) error {
			return runVerifyWorkspace(taskContext, workspace)
		})
	case "test":
		var errs error
		for _, suite := range suites {
			if suite != "" {
				cli.Header(2, "CI test suite: %s", suite)
			}
			options := commandScheduleOptions(true, phase, suite, reporter)
			suiteErr := CIWithPlanOptions(ctx, workspace, plan, runTestServiceForSuite(suite, failFast), options)
			if suiteErr != nil {
				errs = errors.Join(errs, suiteErr)
				if failFast {
					return errs
				}
			}
			if ctx.Err() != nil {
				if !errors.Is(suiteErr, ctx.Err()) {
					errs = errors.Join(errs, ctx.Err())
				}
				return errs
			}
		}
		return errs
	default:
		action := runPhaseAction(phase)
		options := commandScheduleOptions(phaseLocksDependencyClosure(phase), phase, "", reporter)
		return CIWithPlanOptions(ctx, workspace, plan, action, options)
	}
}

func normalizeTestSuites(suites []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(suites))
	for _, suite := range suites {
		suite = strings.TrimSpace(suite)
		if suite == "" || seen[suite] {
			continue
		}
		seen[suite] = true
		result = append(result, suite)
	}
	if len(result) == 0 {
		return []string{""}
	}
	return result
}

func normalizeRunPhases(phases []string) ([]string, error) {
	if len(phases) == 0 {
		phases = []string{"verify", "sync-drift", "lint", "compile", "test", "audit", "sbom", "build"}
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(phases))
	for _, raw := range phases {
		for _, phase := range strings.Split(raw, ",") {
			phase = strings.ToLower(strings.TrimSpace(phase))
			if phase == "" || seen[phase] {
				continue
			}
			switch phase {
			case "verify", "sync-drift", "lint", "compile", "test", "audit", "sbom", "build":
			default:
				return nil, fmt.Errorf("unsupported CI phase %q (use verify, sync-drift, lint, compile, test, audit, sbom, or build)", phase)
			}
			seen[phase] = true
			result = append(result, phase)
		}
	}
	return result, nil
}

func runPhaseAction(phase string) Action {
	switch phase {
	case "lint":
		return runLintService
	case "compile":
		return runCompileService
	case "sync-drift":
		return runSyncDriftService
	case "test":
		return runTestService
	case "audit":
		return runAuditService
	case "sbom":
		return runSBOMService
	case "build":
		return runBuildService
	default:
		panic("validated CI phase has no action: " + phase)
	}
}

func phaseLocksDependencyClosure(phase string) bool {
	return phase == "sync-drift" || phase == "test"
}

func runVerifyWorkspace(ctx context.Context, workspace *resources.Workspace) error {
	report, err := integrity.VerifyBase(ctx, workspace)
	recordCIReportIntegrity(ctx, summarizeIntegrityReport(report))
	return err
}

func summarizeIntegrityReport(report integrity.BaseReport) CIReportIntegrity {
	summary := CIReportIntegrity{
		GuardedModules: len(report.Modules),
		FailedModules:  report.Failed(),
		Modules:        make([]CIReportIntegrityModule, 0, len(report.Modules)),
	}
	for _, module := range report.Modules {
		item := CIReportIntegrityModule{
			Module:   module.Module,
			Files:    module.Files,
			Omitted:  make(map[string]int, len(module.Omitted)),
			Allowed:  make([]CIReportIntegrityDivergence, 0, len(module.Allowed)),
			Missing:  cloneStrings(module.Missing),
			Modified: cloneStrings(module.Modified),
			Error:    module.Error,
		}
		for service, count := range module.Omitted {
			item.Omitted[service] = count
		}
		for _, allowed := range module.Allowed {
			item.Allowed = append(item.Allowed, CIReportIntegrityDivergence{Path: allowed.Path, Reason: allowed.Reason})
		}
		summary.Modules = append(summary.Modules, item)
	}
	return summary
}

func init() {
	runSelection.Bind(RunCmd)
	RunCmd.Flags().StringSliceVar(&runPhases, "phase", nil, "CI phase to run (repeatable or comma-separated; default: full Codefly gate)")
	RunCmd.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent services")
	RunCmd.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for validation and tests")
	RunCmd.Flags().StringSliceVar(&testSuites, "suite", nil, "Named test suite to run during the test phase (repeatable; default: each agent's advertised default)")
	RunCmd.Flags().BoolVar(&ciAuditIncludeOutdated, "audit-outdated", true, "Include outdated dependencies in audit evidence")
	RunCmd.Flags().BoolVar(&ciAuditIncludeDev, "audit-include-dev", false, "Include development/test-only dependencies in audit evidence")
	RunCmd.Flags().BoolVar(&ciAuditFailOnVuln, "fail-on-vuln", true, "Fail audit on HIGH or CRITICAL vulnerabilities")
	RunCmd.Flags().BoolVar(&ciSBOMIncludeDev, "sbom-include-dev", true, "Include development/test dependencies in CI SBOMs")
	bindSchedulingFlags(RunCmd)
	bindReportFlags(RunCmd)
}
