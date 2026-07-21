package ci

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/validation"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

var (
	lintSelection    SelectionFlags
	compileSelection SelectionFlags
)

var LintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Lint affected services through their agents",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return runValidationCommand(&lintSelection, runLintService, "lint")
	},
}

var CompileCmd = &cobra.Command{
	Use:   "compile",
	Short: "Compile or type-check affected services through their agents",
	Args:  cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		return runValidationCommand(&compileSelection, runCompileService, "compile")
	},
}

func runValidationCommand(selection *SelectionFlags, action Action, phase string) error {
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
	plan, err := selection.BuildPlan(ctx, workspace)
	if err != nil {
		return fmt.Errorf("cannot build affected-service plan: %w", err)
	}
	return runWithCIReport(ctx, workspace, plan, "codefly ci "+phase, func(reporter *CIReporter) error {
		options := commandScheduleOptions(false, phase, "", reporter)
		if err := CIWithPlanOptions(ctx, workspace, plan, action, options); err != nil {
			return fmt.Errorf("cannot run CI %s: %w", phase, err)
		}
		return ctx.Err()
	})
}

func runLintService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	return validation.RunService(ctx, workspace, module, service, orchestration.LintMode, "lint", runtimeContext)
}

func runCompileService(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) error {
	return validation.RunService(ctx, workspace, module, service, orchestration.CompileMode, "compile", runtimeContext)
}

func init() {
	lintSelection.Bind(LintCmd)
	compileSelection.Bind(CompileCmd)
	for _, command := range []*cobra.Command{LintCmd, CompileCmd} {
		command.Flags().StringSliceVar(&silent, "silent", []string{}, "Silent services")
		command.Flags().StringVar(&runtimeContext, "runtime-context", "free", "Runtime context for validation")
		bindSchedulingFlags(command)
		bindReportFlags(command)
	}
}
