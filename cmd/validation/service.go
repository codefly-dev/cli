// Package validation exposes local, plugin-owned development operations.
// CI imports the same dispatch helper and adds only selection and scheduling.
package validation

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var (
	lintRuntimeContext    string
	compileRuntimeContext string
)

// LintServiceCmd runs the loaded service plugin's Runtime.Lint RPC.
var LintServiceCmd = newServiceCommand(
	"Lint a service through its plugin",
	"lint",
	orchestration.LintMode,
	&lintRuntimeContext,
)

// CompileServiceCmd runs the loaded service plugin's Runtime.Build RPC. This
// native compile/typecheck operation is distinct from deployable Builder.Build.
var CompileServiceCmd = newServiceCommand(
	"Compile or typecheck a service through its plugin",
	"compile",
	orchestration.CompileMode,
	&compileRuntimeContext,
)

func newServiceCommand(short, operation string, mode orchestration.Mode, runtimeContext *string) *cobra.Command {
	command := &cobra.Command{
		Use:   "service [module/]service",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()

			cli.Init()
			defer services.ClearAgents()

			workspace, module, service, err := common.LoadRequiredE(ctx, args)
			if err != nil {
				return fmt.Errorf("cannot load required service: %w", err)
			}
			if err := RunService(ctx, workspace, module, service, mode, operation, *runtimeContext); err != nil {
				return err
			}
			cli.Header(1, "%s passed for %s", operation, resources.WithUnique(service).Unique())
			return nil
		},
	}
	command.Flags().StringVar(runtimeContext, "runtime-context", "free", "Runtime context for validation")
	return command
}

// RunService dispatches one static validation operation to the service's
// Runtime plugin. It is deliberately independent of CI: callers may be a local
// command, CI scheduler, editor, or Mind integration.
func RunService(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	service *resources.Service,
	mode orchestration.Mode,
	operation string,
	runtimeContext string,
) (result error) {
	w := wool.Get(ctx).In("validateService", wool.ThisField(resources.WithUnique(service)))
	if err := resources.ValidateRuntimeContext(runtimeContext); err != nil {
		return w.NewError("invalid runtime context: %s", runtimeContext)
	}

	env, err := orchestration.SelectEnvironment(workspace, orchestration.LocalEnvironmentName)
	if err != nil {
		return w.Wrap(err)
	}

	flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, mode)
	if err != nil {
		return w.Wrap(err)
	}
	defer func() {
		if stopErr := flow.Stop(); stopErr != nil {
			result = errors.Join(result, fmt.Errorf("cannot stop validation flow: %w", stopErr))
		}
		for _, cacheKey := range flow.AgentCacheKeys() {
			services.ClearAgent(cacheKey)
		}
	}()

	// Static validation needs the target's source and toolchain, not live
	// databases, caches, or application dependencies.
	flow.WithStandAlone(true)
	flow.WithRuntimeContext(runtimeContext)
	if err := flow.InitManagers(ctx); err != nil {
		return w.Wrap(err)
	}
	if err := flow.Load(ctx); err != nil {
		return w.Wrap(err)
	}
	if err := flow.Start(ctx); err != nil {
		return w.Wrapf(err, "cannot %s service", operation)
	}
	return nil
}
